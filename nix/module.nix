{ self }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.coolix;
  caddyDir = "/run/coolix/caddy";
  querySocket = "/run/coolix/query.sock";
  controlSocket = "/run/coolix/control.sock";
  uiSocketPath = "/run/coolix/ui.sock";
  uiSocket = cfg.ui.listen == null && (cfg.caddy.enable || cfg.ui.socketGroup != null);
  uiSocketGroup = if cfg.ui.socketGroup != null then cfg.ui.socketGroup else config.services.caddy.group;
  externalUrl = if cfg.ui.externalUrl == null then null else lib.removeSuffix "/" cfg.ui.externalUrl;
  uiListen = if cfg.ui.listen != null then cfg.ui.listen else "127.0.0.1:8090";
  # Rendered units call the (unwrapped) coolix binary through this stable
  # path, so upgrading coolix doesn't change, and restart, every app unit.
  execPath = "/etc/coolix/coolix";
  # Subdirectories of services.coolix.dataDir, mounted at /var/lib/private/coolix-<name>.
  dataDirs = [ "apps" "db" "backups" ];

  env = {
    COOLIX_STATE_DIR = cfg.stateDir;
    COOLIX_SECRETS_DIR = cfg.secretsDir;
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
    COOLIX_CONTROL_SOCKET = controlSocket;
    COOLIX_ALLOWED_HARDENING = lib.concatStringsSep " " cfg.apps.allowedHardeningOverrides;
    COOLIX_DEFAULT_TASKS_MAX = toString cfg.apps.defaultTasksMax;
    # The host's own Caddy sites can't be claimed by apps.
    COOLIX_RESERVED_DOMAINS = lib.concatStringsSep " " (lib.concatLists (lib.mapAttrsToList
      (name: vh: lib.splitString "," name ++ lib.splitString "," vh.hostName ++ vh.serverAliases)
      config.services.caddy.virtualHosts) ++ lib.optional (externalUrl != null) externalUrl);
  } // lib.optionalAttrs (cfg.apps.defaultMemoryMax != null) {
    COOLIX_DEFAULT_MEMORY_MAX = cfg.apps.defaultMemoryMax;
  } // lib.optionalAttrs (cfg.ui.domain != null) {
    COOLIX_UI_DOMAIN = cfg.ui.domain;
  } // lib.optionalAttrs (cfg.previews.domain != null) {
    COOLIX_PREVIEW_DOMAIN = cfg.previews.domain;
  } // lib.optionalAttrs cfg.git.enable {
    COOLIX_GIT_HOST = cfg.git.host;
    COOLIX_GIT_PORT = toString cfg.git.port;
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

    dataDir = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = "/srv/coolix";
      description = ''
        Where app data, databases and database backups live, e.g. on a
        bigger disk: `<dataDir>/apps/<app>`, `<dataDir>/db/<app>-<kind>` and
        `<dataDir>/backups/<app>`. Apps run as systemd dynamic users, whose
        state systemd keeps in `/var/lib/private`; coolix bind-mounts these
        directories there, so the sandboxing is unchanged. `null` keeps
        everything in `/var/lib`.
      '';
    };

    secretsDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/coolix-secrets";
      description = "Where root keeps the apps' encrypted secrets (`<app>.cred`, readable by root only).";
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

    apps = {
      allowedHardeningOverrides = lib.mkOption {
        type = lib.types.listOf (lib.types.enum [
          "MemoryDenyWriteExecute" "PrivateDevices" "PrivateTmp" "ProtectHome" "ProtectClock"
          "ProtectHostname" "ProtectKernelLogs" "ProtectKernelModules" "ProtectKernelTunables"
          "ProtectControlGroups" "RestrictNamespaces" "RestrictRealtime" "LockPersonality"
          "RestrictAddressFamilies" "SystemCallFilter" "UMask"
        ]);
        default = [
          "MemoryDenyWriteExecute" "PrivateTmp" "ProtectHome" "ProtectHostname" "ProtectClock"
          "RestrictRealtime" "LockPersonality" "UMask"
        ];
        description = ''
          `[hardening]` keys apps may set in `app.toml`. None of them can grant
          privileges, but the ones left out by default (devices, kernel
          interfaces, user namespaces) widen the kernel attack surface
          reachable from an app. `RestrictAddressFamilies` and
          `SystemCallFilter` can only add to coolix's defaults.
        '';
      };
      defaultTasksMax = lib.mkOption {
        type = lib.types.ints.unsigned;
        default = 1024;
        description = "`TasksMax` for apps that don't set `resources.tasks_max` (0: systemd's default).";
      };
      defaultMemoryMax = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "1G";
        description = "`MemoryMax` for apps that don't set `resources.memory_max`.";
      };
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

    previews.domain = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "preview.example.com";
      description = ''
        Previews of branches and pull requests (apps with `[preview] enabled`)
        are served at `<app>-<branch or prN>.<domain>`, e.g.
        `blog-pr12.preview.example.com`. Point a wildcard DNS record at the
        host. Apps can set their own `preview.domain`.
      '';
    };

    git = {
      enable = lib.mkEnableOption ''
        deploys by `git push` over SSH (to `coolix@host:APP`) for apps with
        `source = "push"`. Needs `services.openssh`. The coolix user's SSH
        logins can only run git push and fetch for push apps: sshd forces
        `coolix ssh-serve` and forbids terminals and forwarding
      '';
      host = lib.mkOption {
        type = lib.types.str;
        default = if cfg.ui.domain != null then cfg.ui.domain else config.networking.fqdnOrHostName;
        defaultText = lib.literalExpression "if ui.domain != null then ui.domain else networking.fqdnOrHostName";
        description = "Host name shown in push remotes (`coolix@<host>:APP`).";
      };
      port = lib.mkOption {
        type = lib.types.port;
        default = lib.head (config.services.openssh.ports ++ [ 22 ]);
        defaultText = lib.literalExpression "head services.openssh.ports";
        description = "SSH port shown in push remotes.";
      };
      authorizedKeys = lib.mkOption {
        type = lib.types.listOf lib.types.singleLineStr;
        default = [ ];
        example = [ "ssh-ed25519 AAAAC3Nza... alice@laptop" ];
        description = ''
          SSH public keys that may push to every push app. More keys can be
          added in the web UI (account page) or with `coolix ssh-key add`.
        '';
      };
    };

    ui = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Run the web UI (`coolix serve`) as the coolix user.";
      };
      listen = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "127.0.0.1:8090";
        description = ''
          TCP address for the web UI. By default (`null`) it listens on
          `/run/coolix/ui.sock`, which only Caddy and root can open: apps share
          the host network, and could otherwise reach the UI directly. Without
          Caddy, the default is `127.0.0.1:8090`. For passkeys over an SSH
          tunnel, forward `localhost:8090` to the socket
          (`ssh -L 8090:/run/coolix/ui.sock host`).
        '';
      };
      domain = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "coolix.example.com";
        description = "Serve the web UI on this domain through Caddy (requires `caddy.enable`).";
      };
      externalUrl = lib.mkOption {
        type = lib.types.nullOr (lib.types.strMatching "https?://[A-Za-z0-9.-]+(:[0-9]+)?/?");
        default = null;
        example = "https://coolix.example.com";
        description = ''
          The UI's public URL when your own reverse proxy (e.g. nginx) serves
          it instead of `ui.domain`. Passkeys work on this URL, cookies are
          `Secure` when it's HTTPS, and apps can't claim its domain. Point the
          proxy at the UI's socket (see `ui.socketGroup`) rather than a TCP
          port: apps share the host network and could reach a TCP port
          directly.
        '';
      };
      socketGroup = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "nginx";
        description = ''
          Group that may connect to the UI's socket, `/run/coolix/ui.sock`
          (default: Caddy's group). Set it to your reverse proxy's group,
          e.g. `nginx` with `proxyPass = "http://unix:/run/coolix/ui.sock"`.
        '';
      };
      adminPasswordFile = lib.mkOption {
        type = lib.types.nullOr lib.types.path;
        default = null;
        example = "/run/secrets/coolix-admin-password";
        description = ''
          Optional file containing an admin password for the web UI (at least
          12 characters), for declarative setups (e.g. from sops-nix or agenix).
          Read through systemd's LoadCredential, so it may be root-only. Don't
          use a path inside the Nix store.

          Without it, the first visit shows a setup page that needs a one-time
          code from `sudo coolix setup-code`, where you add a passkey and/or a
          password.
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
        assertion = cfg.git.enable -> config.services.openssh.enable;
        message = "services.coolix.git.enable requires services.openssh.enable";
      }
      {
        assertion = cfg.ui.domain != null -> cfg.caddy.enable;
        message = "services.coolix.ui.domain requires services.coolix.caddy.enable";
      }
      {
        assertion = cfg.portRange.to < 32768;
        message = "services.coolix.portRange must end below 32768 (the kernel's ephemeral ports, which apps could otherwise grab)";
      }
      {
        assertion = cfg.dataDir == null || !(lib.hasPrefix "${toString cfg.stateDir}/" "${toString cfg.dataDir}/");
        message = "services.coolix.dataDir must not be inside stateDir (which the coolix user owns)";
      }
      {
        assertion = !(lib.hasPrefix "${toString cfg.stateDir}/" "${toString cfg.secretsDir}/");
        message = "services.coolix.secretsDir must not be inside stateDir (which the coolix user owns)";
      }
    ];

    nix.settings.experimental-features = [ "nix-command" "flakes" ];

    environment.systemPackages = [ coolix ];
    # The unwrapped binary: units call it with everything as arguments.
    environment.etc."coolix/coolix".source = "${cfg.package}/bin/coolix";

    # Owns the apps repo and runs builds; the web UI runs as this user. It
    # can deploy any code into an app's sandbox, but not change the sandbox:
    # root validates every profile and renders the units itself. It reaches
    # root only through coolix-control, and reads logs and backup listings
    # through coolix-query, not the systemd-journal group.
    users.users.coolix = {
      isSystemUser = true;
      group = "coolix";
      home = cfg.stateDir;
      # sshd runs its ForceCommand through the login shell.
      shell = lib.mkIf cfg.git.enable pkgs.bashInteractive;
      openssh.authorizedKeys.keys = map (k: "restrict ${k}") cfg.git.authorizedKeys;
    };

    # git push over SSH: every login of the coolix user runs `coolix
    # ssh-serve`, which only answers git requests for push apps. Keys come
    # from NixOS (services.coolix.git.authorizedKeys) and from the UI,
    # through AuthorizedKeysCommand. sshd refuses to run that from the
    # group-writable /nix/store, so it's a copied script in /etc.
    environment.etc."coolix/ssh-authorized-keys" = lib.mkIf cfg.git.enable {
      mode = "0555";
      text = ''
        #!${pkgs.runtimeShell}
        exec ${cfg.package}/bin/coolix ssh-authorized-keys ${lib.escapeShellArg cfg.stateDir}
      '';
    };
    services.openssh.extraConfig = lib.mkIf cfg.git.enable (lib.mkAfter ''
      Match User coolix
        AuthorizedKeysFile /etc/ssh/authorized_keys.d/coolix
        AuthorizedKeysCommand /etc/coolix/ssh-authorized-keys
        AuthorizedKeysCommandUser coolix
        AuthenticationMethods publickey
        ExposeAuthInfo yes
        ForceCommand ${coolix}/bin/coolix ssh-serve
        PermitTTY no
        PermitUserRC no
        DisableForwarding yes
      Match all
    '');
    users.groups.coolix = { };

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0750 coolix coolix -"
      "d /nix/var/nix/profiles/coolix 0755 root root -"
      # Encrypted app secrets; written only by the root activator.
      "d ${cfg.secretsDir} 0700 root root -"
      "d /run/coolix 0755 root root -"
    ] ++ lib.optional cfg.caddy.enable "d ${caddyDir} 0755 root root -";

    # services.coolix.dataDir: bind-mount <dataDir>/{apps,db,backups} where
    # systemd keeps DynamicUser state, before anything starts.
    systemd.services.coolix-datadir = lib.mkIf (cfg.dataDir != null) {
      description = "Create coolix data directories";
      unitConfig = {
        DefaultDependencies = false;
        RequiresMountsFor = [ "/var/lib" cfg.dataDir ];
      };
      wantedBy = [ "local-fs.target" ];
      before = map (d: "var-lib-private-coolix\\x2d${d}.mount") dataDirs ++ [ "local-fs.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        # Like /var/lib/private: the outer directory is root-only, which
        # protects everything inside; the mounted subdirectories are 0755 as
        # systemd makes them (coolix-query lists backups through a bind mount).
        ExecStart = [
          "${pkgs.coreutils}/bin/install -d -m 0700 /var/lib/private ${cfg.dataDir}"
          "${pkgs.coreutils}/bin/install -d -m 0755 ${lib.concatMapStringsSep " " (d: "${cfg.dataDir}/${d}") dataDirs}"
        ];
      };
    };
    systemd.mounts = lib.optionals (cfg.dataDir != null) (map
      (d: {
        description = "coolix ${d} on ${cfg.dataDir}";
        what = "${cfg.dataDir}/${d}";
        where = "/var/lib/private/coolix-${d}";
        type = "none";
        options = "bind";
        wantedBy = [ "local-fs.target" ];
        requires = [ "coolix-datadir.service" ];
        after = [ "coolix-datadir.service" ];
      })
      dataDirs);

    # Privileged requests (activate a built profile, back up or restore an
    # app's database) go through this socket, which only root and the coolix
    # group can open. Each connection gets a root coolix-control@ instance
    # that validates the request and the profile before doing anything. The
    # coolix user gets no polkit rights at all: polkit can't tell "start an
    # existing unit" from "create a transient unit with that name".
    systemd.sockets.coolix-control = {
      description = "coolix control socket";
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = controlSocket;
        Accept = true;
        SocketUser = "root";
        SocketGroup = "coolix";
        SocketMode = "0660";
        MaxConnections = 16;
      };
    };
    systemd.services."coolix-control@" = {
      description = "coolix control request";
      # Don't restart running requests on nixos-rebuild switch.
      restartIfChanged = false;
      serviceConfig = {
        ExecStart = "${coolix}/bin/coolix control-serve";
        StandardInput = "socket";
        StandardOutput = "socket";
        StandardError = "journal";
        ProtectHome = true;
        PrivateTmp = true;
        RestrictAddressFamilies = "AF_UNIX AF_INET AF_INET6 AF_NETLINK";
        LockPersonality = true;
        RestrictRealtime = true;
      };
    };

    # Read-only answers for the coolix user: app logs and backup listings.
    # One instance per connection, as a dynamic user that may read the
    # journal and (via a read-only bind mount) the backup directories, and
    # nothing else. The backups live where systemd keeps the state of
    # DynamicUser units (the backup units run as the app's dynamic user);
    # the "-" ignores the path until the first backup exists, and every
    # connection is a new instance, so it shows up then.
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
      restartIfChanged = false;
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
        ProtectProc = "invisible";
        UMask = "0077";
      };
    };

    # App units live in /run/systemd/system (outside NixOS' control), so they
    # need to be re-rendered from the current app profile on every boot.
    # Apps that no longer pass the policy are skipped, and a profile from an
    # older coolix only logs a warning: boot never fails because of coolix.
    systemd.services.coolix-activate-boot = {
      description = "Activate the current coolix app profile";
      # Only at boot: on nixos-rebuild switch, running apps are left alone
      # (units are re-rendered by the next deploy).
      restartIfChanged = false;
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ] ++ lib.optional cfg.caddy.enable "caddy.service";
      after = [ "network-online.target" "local-fs.target" ] ++ lib.optional cfg.caddy.enable "caddy.service";
      unitConfig.ConditionPathExists = "/nix/var/nix/profiles/coolix/apps";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${coolix}/bin/coolix activate";
      };
    };

    # The UI's socket: only root and Caddy can connect.
    systemd.sockets.coolix = lib.mkIf (cfg.ui.enable && uiSocket) {
      description = "coolix web UI socket";
      wantedBy = [ "sockets.target" ];
      listenStreams = [ uiSocketPath ];
      socketConfig = {
        SocketUser = "root";
        SocketGroup = uiSocketGroup;
        SocketMode = "0660";
      };
    };
    systemd.services.coolix = lib.mkIf cfg.ui.enable {
      description = "coolix web UI";
      wantedBy = [ "multi-user.target" ];
      requires = lib.optional uiSocket "coolix.socket";
      after = [ "network.target" ] ++ lib.optional uiSocket "coolix.socket";
      environment = {
        COOLIX_LISTEN = if uiSocket then "" else uiListen;
        COOLIX_SECURE_COOKIES =
          if (cfg.ui.domain != null && cfg.caddy.tls != "off") || lib.hasPrefix "https://" (toString externalUrl)
          then "1" else "0";
        # Passkeys are bound to these origins: the UI's domain, and localhost
        # (e.g. through an SSH tunnel).
        COOLIX_WEBAUTHN_ORIGINS = lib.concatStringsSep " " (
          lib.optional (externalUrl != null) externalUrl
          ++ lib.optional (cfg.ui.domain != null)
            "${if cfg.caddy.tls == "off" then "http" else "https"}://${cfg.ui.domain}"
          ++ [ "http://localhost:${if uiSocket then "8090" else lib.last (lib.splitString ":" uiListen)}" ]
        );
      } // lib.optionalAttrs (cfg.ui.adminPasswordFile != null) {
        COOLIX_ADMIN_PASSWORD_FILE = "%d/password";
      };
      serviceConfig = {
        User = "coolix";
        Group = "coolix";
        ExecStart = "${coolix}/bin/coolix serve";
        LoadCredential = lib.mkIf (cfg.ui.adminPasswordFile != null) "password:${cfg.ui.adminPasswordFile}";
        Restart = "on-failure";
        # Builds go through the nix daemon and privileged requests through
        # coolix-control, so the UI itself needs very little.
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
        ProtectKernelLogs = true;
        ProtectClock = true;
        ProtectHostname = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        CapabilityBoundingSet = "";
        SystemCallArchitectures = "native";
        RestrictAddressFamilies = "AF_UNIX AF_INET AF_INET6";
      };
    };

    services.caddy = lib.mkIf cfg.caddy.enable {
      enable = true;
      email = cfg.caddy.acmeEmail;
      # Apps share the host network; Caddy's admin API must not be reachable
      # from them (by default it listens on localhost:2019, unauthenticated).
      # A unix socket in a caddy-only directory keeps `caddy reload` working.
      globalConfig = ''
        admin unix//run/caddy/admin.sock
      '';
      extraConfig = ''
        import ${caddyDir}/*.caddy
      '';
      virtualHosts = lib.mkIf (cfg.ui.domain != null) {
        "${lib.optionalString (cfg.caddy.tls == "off") "http://"}${cfg.ui.domain}".extraConfig = ''
          ${lib.optionalString (cfg.caddy.tls == "internal") "tls internal"}
          reverse_proxy ${if uiSocket then "unix/${uiSocketPath}" else uiListen}
        '';
      };
    };
    systemd.services.caddy.serviceConfig = lib.mkIf cfg.caddy.enable {
      RuntimeDirectory = "caddy";
      RuntimeDirectoryMode = "0700";
    };
    networking.firewall.allowedTCPPorts = lib.mkIf (cfg.caddy.enable && cfg.caddy.openFirewall) [ 80 443 ];
  };
}
