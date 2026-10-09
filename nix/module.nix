{
  config,
  lib,
  utils,
  ...
}:
let
  cfg = config.services.nefit-go;
  # No IPv6 zones: resolving one needs netlink, which the unit denies.
  address = lib.types.strMatching "(\\[[0-9A-Fa-f:.]+]|[[:alnum:]_.-]*):[0-9]{1,5}" // {
    description = "host:port with a numeric port (IPv6 hosts in brackets, empty host for all addresses)";
  };
  # The DNS endpoint takes literal addresses only.
  ipAddress = lib.types.strMatching "(\\[[0-9A-Fa-f:.]+]|[0-9.]+):[0-9]{1,5}" // {
    description = "IP:port (IPv6 in brackets)";
  };
  # [ host port ] of an address-typed value.
  split = value: builtins.match "\\[?([^]]*)]?:([0-9]+)" value;
  host = value: lib.head (split value);
  port = value: lib.toIntBase10 (lib.last (split value));
  unspecified =
    value:
    lib.elem (host value) [
      ""
      "0.0.0.0"
      "::"
    ];
  # Only grant low-port binding when a configured listener needs it.
  lowPort = lib.any (value: port value > 0 && port value < 1024) (
    [
      cfg.xmppAddress
      cfg.httpAddress
    ]
    ++ lib.optional cfg.dns.enable cfg.dns.listen
  );
in
{
  options.services.nefit-go = {
    enable = lib.mkEnableOption "Nefit device XMPP server";
    package = lib.mkOption {
      type = lib.types.package;
      description = "Package containing the nefit CLI.";
    };
    environmentFile = lib.mkOption {
      type = lib.types.addCheck lib.types.str (lib.hasPrefix "/");
      description = "Absolute runtime path to a private file containing NEFIT_SERIAL_NUMBER, NEFIT_ACCESS_KEY and NEFIT_PASSWORD. Use a quoted string so credentials stay outside the Nix store.";
    };
    mode = lib.mkOption {
      type = lib.types.enum [
        "offline"
        "both"
      ];
      default = "offline";
      description = "Local service or local service with Bosch forwarding.";
    };
    deviceIP = lib.mkOption {
      type = lib.types.str;
      description = "Expected thermostat source IP.";
    };
    xmppAddress = lib.mkOption {
      type = address;
      default = "127.0.0.1:5222";
      description = "Device XMPP listener address. With DNS enabled, its host must be dns.address or unspecified.";
    };
    httpAddress = lib.mkOption {
      type = address;
      default = "127.0.0.1:8088";
      description = "HTTP API listener address.";
    };
    httpAllowedHosts = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Additional trusted HTTP hostnames or host:port authorities for LAN DNS and authenticated proxies.";
    };
    domain = lib.mkOption {
      type = lib.types.str;
      default = "wa2-mz36-qrmzh6.bosch.de";
      description = "Original Bosch XMPP domain.";
    };
    upstream = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Bosch host:port or resolved IP:port to bypass DNS overrides. Requires mode = \"both\".";
    };
    updates = lib.mkOption {
      type = lib.types.enum [
        "allow"
        "block"
      ];
      default = "allow";
      description = "Policy for update-related XMPP traffic.";
    };
    updateServices = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Additional update service JIDs/localparts to block. Requires updates = \"block\".";
    };
    requestTimeout = lib.mkOption {
      # Go durations in these units are also systemd time spans.
      type = lib.types.strMatching "([0-9]+(\\.[0-9]+)?(h|m|s|ms|us|µs|μs))+" // {
        description = "Go duration in h, m, s, ms or us, such as 30s or 1m30s";
      };
      default = "30s";
      description = "CLI --timeout: bounds each device request attempt, the HTTP handler, and how long the device may take to answer.";
    };
    dns = {
      enable = lib.mkEnableOption "device-scoped DNS endpoint for the Bosch hostname";
      listen = lib.mkOption {
        type = ipAddress;
        example = "192.0.2.1:53";
        description = "UDP/TCP DNS listener: specific IP:port (no 0.0.0.0/[::]). Answers only deviceIP.";
      };
      address = lib.mkOption {
        type = lib.types.str;
        example = "192.0.2.1";
        description = "Local IP returned for the Bosch hostname.";
      };
      forward = lib.mkOption {
        type = lib.types.nullOr ipAddress;
        default = null;
        example = "192.0.2.53:53";
        description = "Resolver IP:port for unrelated names. Unset refuses them.";
      };
    };
  };
  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = !lib.hasPrefix "${builtins.storeDir}/" cfg.environmentFile;
        message = "services.nefit-go.environmentFile must not be in the Nix store, which is world-readable.";
      }
      {
        assertion = cfg.updateServices == [ ] || cfg.updates == "block";
        message = "services.nefit-go.updateServices requires updates = \"block\".";
      }
      {
        assertion = cfg.upstream == "" || cfg.mode == "both";
        message = "services.nefit-go.upstream requires mode = \"both\".";
      }
      {
        assertion = !cfg.dns.enable || !unspecified cfg.dns.listen;
        message = "services.nefit-go.dns.listen needs a specific IP: a wildcard UDP socket may answer from the wrong address.";
      }
      {
        assertion = !cfg.dns.enable || cfg.dns.forward != cfg.dns.listen;
        message = "services.nefit-go.dns.forward must not be dns.listen.";
      }
      {
        assertion =
          !cfg.dns.enable || unspecified cfg.xmppAddress || host cfg.xmppAddress == cfg.dns.address;
        message = "services.nefit-go.xmppAddress must listen on dns.address or all addresses, since DNS sends the device there.";
      }
    ];
    users.users.nefit-go = {
      isSystemUser = true;
      group = "nefit-go";
    };
    users.groups.nefit-go = { };
    systemd.services.nefit-go = {
      description = "Nefit device XMPP server";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      serviceConfig = {
        User = "nefit-go";
        Group = "nefit-go";
        EnvironmentFile = cfg.environmentFile;
        ExecStart = utils.escapeSystemdExecArgs (
          [
            "${cfg.package}/bin/nefit"
            "--timeout"
            cfg.requestTimeout
            "serve"
            "--mode"
            cfg.mode
            "--device-ip"
            cfg.deviceIP
            "--xmpp-listen"
            cfg.xmppAddress
            "--http-listen"
            cfg.httpAddress
            "--domain"
            cfg.domain
            "--updates"
            cfg.updates
          ]
          ++ lib.optionals (cfg.upstream != "") [
            "--upstream"
            cfg.upstream
          ]
          ++ lib.optionals (cfg.httpAllowedHosts != [ ]) [
            "--http-allowed-hosts"
            (lib.concatStringsSep "," cfg.httpAllowedHosts)
          ]
          ++ lib.optionals (cfg.updateServices != [ ]) [
            "--update-services"
            (lib.concatStringsSep "," cfg.updateServices)
          ]
          ++ lib.optionals cfg.dns.enable [
            "--dns-listen"
            cfg.dns.listen
            "--dns-address"
            cfg.dns.address
          ]
          ++ lib.optionals (cfg.dns.enable && cfg.dns.forward != null) [
            "--dns-forward"
            cfg.dns.forward
          ]
        );
        AmbientCapabilities = lib.mkIf lowPort "CAP_NET_BIND_SERVICE";
        CapabilityBoundingSet = if lowPort then "CAP_NET_BIND_SERVICE" else "";
        Restart = "on-failure";
        RestartSec = "5s";
        # The CLI exits 78 (EX_CONFIG) for configuration it rejects; restarting
        # cannot help.
        RestartPreventExitStatus = 78;
        # The CLI drains for --timeout plus 5s; allow 30s more before SIGKILL.
        TimeoutStopSec = "${cfg.requestTimeout} 35s";
        UMask = "0077";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" ];
      };
    };
  };
}
