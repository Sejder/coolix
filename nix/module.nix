{ self }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.coolix;
  caddyDir = "/run/coolix/caddy";
  querySocket = "/run/coolix/query.sock";
  # Rendered units call the (unwrapped) coolix binary through this stable
  # path, so upgrading coolix doesn't change, and restart, every app unit.
  execPath = "/etc/coolix/coolix";

  env = {
    COOLIX_STATE_DIR = cfg.stateDir;
    COOLIX_USER = "coolix";
    COOLIX_NIXPKGS = "${pkgs.path}";
    COOLIX_LIB = "${self}";
    COOLIX_SYSTEM = pkgs.stdenv.hostPlatform.system;
    COOLIX_PORT_MIN = toString cfg.portRange.from;
    COOLIX_PORT_MAX = toString cfg.portRange.to;
    COOLIX_DRAIN = cfg.drainTime;
    COOLIX_KEEP_GENERATIONS = toString cfg.keepGenerations;
    COOLIX_EXEC = execPath;
    COOLIX_QUERY_SOCKET = querySocket;
  } // lib.optionalAttrs (cfg.ui.domain != null) {
    COOLIX_UI_DOMAIN = cfg.ui.domain;
  } // lib.optionalAttrs cfg.caddy.enable {
    COOLIX_CADDY_DIR = caddyDir;
    COOLIX_TLS = cfg.caddy.tls;
  };

  # The CLI is wrapped with everything it needs to know about this host:
  # which nixpkgs/coolix lib the apps repo pins, and the tools it shells out to.
  coolix = pkgs.runCommand "coolix-wrapped" { nativeBuildInputs = [ pkgs.makeWrapper ]; } ''
    makeWrapper ${lib.getExe cfg.package} $out/bin/coolix \
      ${lib.concatStringsSep " " (lib.mapAttrsToList (k: v: "--set ${k} ${lib.escapeShellArg v}") env)} \
      --prefix PATH : ${lib.makeBinPath [ pkgs.git pkgs.util-linux config.nix.package config.systemd.package ]}
  '';
in
{
  options.services.coolix = {
    enable = lib.mkEnableOption "coolix, a NixOS-native self-hosting PaaS";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.coolix;
      defaultText = lib.literalExpression "coolix.packages.\${system}.coolix";
      description = "The coolix package.";
    };

    stateDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/coolix";
      description = "Where coolix keeps the apps repo and operational state.";
    };

    portRange = {
      from = lib.mkOption {
        type = lib.types.port;
        default = 20000;
        description = "First port coolix may allocate to apps (each app takes two: blue and green).";
      };
      to = lib.mkOption {
        type = lib.types.port;
        default = 29999;
        description = "Last port coolix may allocate to apps.";
      };
    };

    drainTime = lib.mkOption {
      type = lib.types.str;
      default = "2s";
      description = "How long the old slot keeps running after traffic moved to the new one (Go duration).";
    };

    keepGenerations = lib.mkOption {
      type = lib.types.ints.unsigned;
      default = 20;
      description = "App profile generations to keep for rollback; older ones become garbage-collectable. 0 keeps all.";
    };

    caddy = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Route app domains through Caddy (enables services.caddy).";
      };
      tls = lib.mkOption {
        type = lib.types.enum [ "acme" "internal" "off" ];
        default = "acme";
        description = ''
          How app domains get certificates: `acme` (Let's Encrypt/ZeroSSL via
          Caddy's automatic HTTPS), `internal` (Caddy's local CA) or `off`
          (plain HTTP).
        '';
      };
      acmeEmail = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Contact email for ACME certificates.";
      };
      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open ports 80 and 443.";
      };
    };

    ui = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Run the web UI (`coolix serve`) as the coolix user.";
      };
      listen = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:8090";
        description = "Address the web UI listens on. Keep it local and expose it through `ui.domain`.";
      };
      domain = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "coolix.example.com";
        description = "Serve the web UI on this domain through Caddy (requires `caddy.enable`).";
      };
      adminPasswordFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        example = "/run/secrets/coolix-admin-password";
        description = ''
          File containing the admin password for the web UI. Read through
          systemd's LoadCredential, so it may be root-only (e.g. from sops-nix
          or agenix). Don't use a path inside the Nix store.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.portRange.from < cfg.portRange.to;
        message = "services.coolix.portRange.from must be lower than portRange.to";
      }
      {
        assertion = cfg.ui.enable -> cfg.ui.adminPasswordFile != null;
        message = "services.coolix.ui.adminPasswordFile must be set when the web UI is enabled";
      }
      {
        assertion = cfg.ui.domain != null -> cfg.caddy.enable;
        message = "services.coolix.ui.domain requires services.coolix.caddy.enable";
      }
    ];

    nix.settings.experimental-features = [ "nix-command" "flakes" ];

    environment.systemPackages = [ coolix ];
    environment.etc."coolix/coolix".source = "${cfg.package}/bin/coolix";

    # Owns the apps repo and runs builds; the web UI runs as this user. It
    # can deploy any code into an app's sandbox, but not change the sandbox:
    # root validates every profile and renders the units itself. It reads
    # logs and backups through coolix-query, not the systemd-journal group.
    users.users.coolix = {
      isSystemUser = true;
      group = "coolix";
      home = cfg.stateDir;
    };
    users.groups.coolix = { };

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0750 coolix coolix -"
      "d /nix/var/nix/profiles/coolix 0755 root root -"
      # Encrypted app secrets; written only by the root activator.
      "d /var/lib/coolix-secrets 0700 root root -"
      "d /run/coolix 0755 root root -"
    ] ++ lib.optional cfg.caddy.enable "d ${caddyDir} 0755 root root -";

    # Privileged half of a deploy. The instance is the profile's store path
    # basename; `coolix activate-store` validates it and the profile's
    # manifest before touching anything.
    systemd.services."coolix-activate@" = {
      description = "Activate coolix app profile %i";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${coolix}/bin/coolix activate-store %i";
      };
    };

    # The coolix user may start activations, and backups/restores of app
    # databases (units root rendered, running as the app's user). Nothing else.
    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (action.id == "org.freedesktop.systemd1.manage-units" &&
            subject.user == "coolix" &&
            action.lookup("verb") == "start") {
          var unit = action.lookup("unit");
          if (/^coolix-activate@[0-9a-df-np-sv-z]{32}-coolix-app-profile\.service$/.test(unit) ||
              /^coolix-backup-[a-z][a-z0-9-]{0,30}[a-z0-9]\.service$/.test(unit) ||
              /^coolix-restore-[a-z][a-z0-9-]{0,30}[a-z0-9]@[0-9]{8}T[0-9]{6}Z\.dump\.service$/.test(unit)) {
            return polkit.Result.YES;
          }
        }
      });
    '';

    # Read-only answers for the coolix user: app logs, activation/backup
    # output and backup listings. One instance per connection, as a dynamic
    # user that may read the journal and (via a read-only bind mount) the
    # backup directories, and nothing else.
    systemd.sockets.coolix-query = {
      description = "coolix query socket";
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = querySocket;
        Accept = true;
        SocketUser = "root";
        SocketGroup = "coolix";
        SocketMode = "0660";
        MaxConnections = 64;
      };
    };
    systemd.services."coolix-query@" = {
      description = "coolix query";
      serviceConfig = {
        ExecStart = "${coolix}/bin/coolix query-serve";
        StandardInput = "socket";
        StandardOutput = "socket";
        StandardError = "journal";
        Environment = "COOLIX_BACKUP_DIR=/run/coolix-backups";
        BindReadOnlyPaths = [ "-/var/lib/private/coolix-backups:/run/coolix-backups" ];
        DynamicUser = true;
        SupplementaryGroups = [ "systemd-journal" ];
        RuntimeMaxSec = "1d";
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        PrivateNetwork = true;
        PrivateDevices = true;
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        RestrictAddressFamilies = "AF_UNIX";
        RestrictNamespaces = true;
        RestrictRealtime = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = "@system-service";
      };
    };

    # App units live in /run/systemd/system (outside NixOS' control), so they
    # need to be re-linked from the current app profile on every boot.
    systemd.services.coolix-activate-boot = {
      description = "Activate the current coolix app profile";
      wantedBy = [ "multi-user.target" ];
      wants = lib.optional cfg.caddy.enable "caddy.service";
      after = [ "network.target" "local-fs.target" ] ++ lib.optional cfg.caddy.enable "caddy.service";
      unitConfig.ConditionPathExists = "/nix/var/nix/profiles/coolix/apps";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${coolix}/bin/coolix activate";
      };
    };

    systemd.services.coolix = lib.mkIf cfg.ui.enable {
      description = "coolix web UI";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      environment = {
        COOLIX_LISTEN = cfg.ui.listen;
        COOLIX_ADMIN_PASSWORD_FILE = "%d/password";
        COOLIX_SECURE_COOKIES = if cfg.ui.domain != null && cfg.caddy.tls != "off" then "1" else "0";
      };
      serviceConfig = {
        User = "coolix";
        Group = "coolix";
        ExecStart = "${coolix}/bin/coolix serve";
        LoadCredential = "password:${cfg.ui.adminPasswordFile}";
        Restart = "on-failure";
        # Builds go through the nix daemon and activation through polkit, so
        # the UI itself needs very little.
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ReadWritePaths = [ cfg.stateDir ];
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
      };
    };

    services.caddy = lib.mkIf cfg.caddy.enable {
      enable = true;
      email = cfg.caddy.acmeEmail;
      extraConfig = ''
        import ${caddyDir}/*.caddy
      '';
      virtualHosts = lib.mkIf (cfg.ui.domain != null) {
        "${lib.optionalString (cfg.caddy.tls == "off") "http://"}${cfg.ui.domain}".extraConfig = ''
          ${lib.optionalString (cfg.caddy.tls == "internal") "tls internal"}
          reverse_proxy ${cfg.ui.listen}
        '';
      };
    };
    networking.firewall.allowedTCPPorts = lib.mkIf (cfg.caddy.enable && cfg.caddy.openFirewall) [ 80 443 ];
  };
}
