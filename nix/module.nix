{ config, lib, ... }:
let
  cfg = config.services.nefit-go;
in
{
  options.services.nefit-go = {
    enable = lib.mkEnableOption "Nefit device XMPP server";
    package = lib.mkOption {
      type = lib.types.package;
      description = "Package containing the nefit CLI.";
    };
    environmentFile = lib.mkOption {
      type = lib.types.path;
      description = "Private file containing NEFIT_SERIAL_NUMBER, NEFIT_ACCESS_KEY and NEFIT_PASSWORD.";
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
      type = lib.types.str;
      default = "127.0.0.1:5222";
      description = "Device XMPP listener address.";
    };
    httpAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8088";
      description = "HTTP API listener address.";
    };
    domain = lib.mkOption {
      type = lib.types.str;
      default = "wa2-mz36-qrmzh6.bosch.de";
      description = "Original Bosch XMPP domain.";
    };
    upstream = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Bosch host:port or resolved IP:port to bypass DNS overrides.";
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
      description = "Additional update service JIDs/localparts to block.";
    };
    requestTimeout = lib.mkOption {
      type = lib.types.str;
      default = "15s";
      description = "Device request timeout.";
    };
  };
  config = lib.mkIf cfg.enable {
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
        ExecStart = lib.escapeShellArgs (
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
          ++ lib.optionals (cfg.updateServices != [ ]) [
            "--update-services"
            (lib.concatStringsSep "," cfg.updateServices)
          ]
        );
        Restart = "on-failure";
        RestartSec = "5s";
        UMask = "0077";
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
      };
    };
  };
}
