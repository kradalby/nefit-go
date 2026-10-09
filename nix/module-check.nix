# Evaluates the NixOS module and asserts the rendered service; nothing is built.
{
  pkgs,
  nixpkgs,
  module,
}:
let
  inherit (nixpkgs) lib;
  eval =
    settings:
    (lib.nixosSystem {
      modules = [
        module
        {
          nixpkgs.hostPlatform = pkgs.stdenv.hostPlatform.system;
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "none";
            fsType = "tmpfs";
          };
          system.stateVersion = lib.trivial.release;
          services.nefit-go = {
            enable = true;
            package = pkgs.emptyDirectory;
            environmentFile = "/run/secrets/nefit-env";
            deviceIP = "192.0.2.10";
          };
        }
        settings
      ];
    }).config;
  service = settings: (eval settings).systemd.services.nefit-go.serviceConfig;
  failures =
    settings:
    map (a: a.message) (
      lib.filter (a: !a.assertion && lib.hasPrefix "services.nefit-go" a.message)
        (eval settings).assertions
    );
  # A definition the option type rejects makes the rendered service throw.
  rejected = settings: !(builtins.tryEval (service settings).ExecStart).success;
  plain = service { };
  dnsSettings = {
    services.nefit-go.xmppAddress = "192.0.2.1:5222";
    services.nefit-go.dns = {
      enable = true;
      listen = "192.0.2.1:53";
      address = "192.0.2.1";
      forward = "192.0.2.53:53";
    };
  };
  dns = service dnsSettings;
  dnsHighPort = service {
    services.nefit-go.xmppAddress = "0.0.0.0:5222";
    services.nefit-go.dns = {
      enable = true;
      listen = "192.0.2.1:5353";
      address = "192.0.2.1";
    };
  };
  proxy = service {
    services.nefit-go.httpAllowedHosts = [
      "nefit.example"
      "proxy.example:8443"
    ];
    services.nefit-go.httpAddress = "127.0.0.1:0";
  };
  has = lib.hasInfix;
  checks = {
    "default timeout matches CLI" = has ''"--timeout" "30s" "serve"'' plain.ExecStart;
    "stop timeout covers CLI drain" = plain.TimeoutStopSec == "30s 35s";
    "configuration errors are not restarted" = plain.RestartPreventExitStatus == 78;
    "custom Go durations render" =
      lib.all
        (
          duration:
          let
            custom = service { services.nefit-go.requestTimeout = duration; };
          in
          has ''"--timeout" "${duration}" "serve"'' custom.ExecStart
          && custom.TimeoutStopSec == "${duration} 35s"
        )
        [
          "2m"
          "1m30.25s"
          "250µs"
        ];
    "systemd specifiers escaped" =
      has ''"%%h$$x"''
        (service { services.nefit-go.domain = "%h$x"; }).ExecStart;
    "no DNS flags by default" = !has "--dns-" plain.ExecStart;
    "no capability on high ports" = !(plain ? AmbientCapabilities) && plain.CapabilityBoundingSet == "";
    "hardened" = plain.SystemCallFilter == [ "@system-service" ] && plain.ProtectProc == "invisible";
    "DNS flags rendered" =
      has ''"--dns-listen" "192.0.2.1:53" "--dns-address" "192.0.2.1" "--dns-forward" "192.0.2.53:53"'' dns.ExecStart;
    "low DNS port grants bind capability" =
      (dns.AmbientCapabilities or null) == "CAP_NET_BIND_SERVICE"
      && dns.CapabilityBoundingSet == "CAP_NET_BIND_SERVICE";
    "unset forward omitted" = !has "--dns-forward" dnsHighPort.ExecStart;
    "high DNS port needs no capability" = !(dnsHighPort ? AmbientCapabilities);
    "ephemeral port needs no capability" = !(proxy ? AmbientCapabilities);
    "trusted HTTP hosts rendered" =
      has ''"--http-allowed-hosts" "nefit.example,proxy.example:8443"'' proxy.ExecStart;
    "credentials stay a runtime path" = plain.EnvironmentFile == "/run/secrets/nefit-env";
    "valid settings pass assertions" = failures dnsSettings == [ ];
    "credentials reject Nix path values" = rejected {
      services.nefit-go.environmentFile = lib.mkForce ./module.nix;
    };
    "credentials reject relative paths" = rejected {
      services.nefit-go.environmentFile = lib.mkForce "secrets/nefit-env";
    };
    "credentials reject store paths" =
      failures { services.nefit-go.environmentFile = lib.mkForce "${builtins.storeDir}/x-env"; } != [ ];
    "addresses need numeric ports and no zone" =
      lib.all (xmppAddress: rejected { services.nefit-go = { inherit xmppAddress; }; })
        [
          "localhost:xmpp"
          "127.0.0.1"
          "::1:5222"
          ""
          "[fe80::1%eth0]:5222"
        ];
    "timeout must be a Go duration" =
      lib.all (requestTimeout: rejected { services.nefit-go = { inherit requestTimeout; }; })
        [
          "30"
          "30 s"
          "1ns"
        ];
    "update services need blocking" =
      failures { services.nefit-go.updateServices = [ "extra" ]; } != [ ];
    "upstream needs both" = failures { services.nefit-go.upstream = "cloud.example:5222"; } != [ ];
    "DNS listen needs a specific IP" =
      lib.all
        (
          listen:
          failures (lib.recursiveUpdate dnsSettings { services.nefit-go.dns = { inherit listen; }; }) != [ ]
        )
        [
          "0.0.0.0:53"
          "[::]:53"
        ];
    "DNS addresses must be literal" =
      lib.all (dns: rejected (lib.recursiveUpdate dnsSettings { services.nefit-go = { inherit dns; }; }))
        [
          { listen = ":53"; }
          { listen = "dns.example:53"; }
          { forward = "resolver.example:53"; }
        ];
    "DNS forward is not the listener" =
      failures (lib.recursiveUpdate dnsSettings { services.nefit-go.dns.forward = "192.0.2.1:53"; })
      != [ ];
    "XMPP must accept DNS address" =
      failures (lib.recursiveUpdate dnsSettings { services.nefit-go.xmppAddress = "127.0.0.1:5222"; })
      != [ ];
  };
  failed = builtins.attrNames (lib.filterAttrs (_: ok: !ok) checks);
in
assert lib.assertMsg (
  failed == [ ]
) "nixos module checks failed: ${lib.concatStringsSep ", " failed}";
pkgs.runCommand "nefit-nixos-module" { } "touch $out"
