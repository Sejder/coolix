{ pkgs, lib, inputs, system, name, dir }:

let
  def = builtins.fromTOML (builtins.readFile (dir + "/app.toml"));

  input = inputs."app-${name}" or (throw
    "coolix: flake input app-${name} is missing; run `coolix deploy` to regenerate flake.nix");
  pkg = input.packages.${system}.${def.package or "default"};
  exe = if def ? command then "${pkg}/${def.command}" else lib.getExe pkg;

  ports = {
    blue = def.port_base;
    green = def.port_base + 1;
  };

  # The app's slots and its databases share one DynamicUser, so databases can
  # use peer authentication on a 0700 socket directory: no passwords at all.
  user = "coolix-app-${name}";

  toUnit = lib.generators.toINI {
    listsAsDuplicateKeys = true;
    mkKeyValue = lib.generators.mkKeyValueDefault
      {
        mkValueString = v:
          if v == true then "yes"
          else if v == false then "no"
          else toString v;
      } "=";
  };

  # Hardened by default; app.toml [hardening] overrides individual keys.
  baseHardening = {
    DynamicUser = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    PrivateTmp = true;
    PrivateDevices = true;
    NoNewPrivileges = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    ProtectClock = true;
    ProtectHostname = true;
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    RestrictAddressFamilies = "AF_INET AF_INET6 AF_UNIX";
    SystemCallArchitectures = "native";
    SystemCallFilter = "@system-service";
    CapabilityBoundingSet = "";
    UMask = "0077";
  };
  hardening = baseHardening // { MemoryDenyWriteExecute = true; } // (def.hardening or { });

  # Databases --------------------------------------------------------------

  dbs = lib.listToAttrs (map (db: lib.nameValuePair db.kind db) (def.databases or [ ]));
  dbUnitName = kind: "coolix-db-${name}-${kind}.service";
  dbDir = kind: "coolix-db/${name}-${kind}";
  socketDir = kind: "/run/${dbDir kind}";

  dbService = kind: extra: toUnit {
    Unit.Description = "coolix ${kind} for ${name}";
    Service = baseHardening // {
      User = user;
      StateDirectory = dbDir kind;
      StateDirectoryMode = "0700";
      RuntimeDirectory = dbDir kind;
      RuntimeDirectoryMode = "0700";
      SyslogIdentifier = "coolix-${name}-${kind}";
      Restart = "on-failure";
    } // extra;
  };

  postgres =
    let
      pg = pkgs."postgresql_${dbs.postgres.version or "18"}";
      init = pkgs.writeShellScript "coolix-${name}-postgres-init" ''
        if [ ! -s "$STATE_DIRECTORY/PG_VERSION" ]; then
          ${pg}/bin/initdb -D "$STATE_DIRECTORY" -U ${user} --auth=peer --encoding=UTF8 --locale=C
        fi
      '';
      createDb = pkgs.writeShellScript "coolix-${name}-postgres-createdb" ''
        psql() { ${pg}/bin/psql -h "$RUNTIME_DIRECTORY" -d postgres -tA "$@"; }
        if [ "$(psql -c "SELECT 1 FROM pg_database WHERE datname = 'app'")" != 1 ]; then
          ${pg}/bin/createdb -h "$RUNTIME_DIRECTORY" app
        fi
      '';
    in
    dbService "postgres" {
      Type = "notify";
      ExecStartPre = "${init}";
      ExecStart = "${pg}/bin/postgres -D %S/${dbDir "postgres"} -k %t/${dbDir "postgres"} -c listen_addresses= -c unix_socket_permissions=0700 -c jit=off";
      ExecStartPost = "${createDb}";
      KillMode = "mixed";
      KillSignal = "SIGINT";
      TimeoutSec = 120;
    };

  redis = dbService "redis" {
    Type = "notify";
    ExecStart = "${pkgs.redis}/bin/redis-server --port 0 --unixsocket %t/${dbDir "redis"}/redis.sock --unixsocketperm 700 --dir %S/${dbDir "redis"} --supervised systemd --daemonize no";
  };

  services = lib.optionalAttrs (dbs ? postgres) { ${dbUnitName "postgres"} = postgres; }
    // lib.optionalAttrs (dbs ? redis) { ${dbUnitName "redis"} = redis; };

  dbEnv = lib.optionalAttrs (dbs ? postgres)
    {
      DATABASE_URL = "postgresql:///app?host=${socketDir "postgres"}";
      PGHOST = socketDir "postgres";
      PGUSER = user;
      PGDATABASE = "app";
    } // lib.optionalAttrs (dbs ? redis) {
    REDIS_URL = "unix://${socketDir "redis"}/redis.sock";
  };

  # Environment ------------------------------------------------------------

  # systemd EnvironmentFile syntax: KEY="value" with \ and " escaped.
  envFile = slot: lib.concatStrings (lib.mapAttrsToList
    (k: v: "${k}=\"${lib.escape [ "\\" "\"" ] (toString v)}\"\n")
    ((def.env or { }) // dbEnv // {
      PORT = ports.${slot};
      COOLIX_APP = name;
      COOLIX_SLOT = slot;
    }));

  envDir = pkgs.runCommandLocal "coolix-${name}-env"
    {
      blue = envFile "blue";
      green = envFile "green";
      passAsFile = [ "blue" "green" ];
    } ''
    mkdir $out
    cp $bluePath $out/blue.env
    cp $greenPath $out/green.env
  '';

  # Secrets: systemd decrypts the credential into $CREDENTIALS_DIRECTORY; the
  # file holds KEY='value' lines, safe to source.
  hasSecrets = (def.secrets or [ ]) != [ ];
  withSecrets = pkgs.writeShellScript "coolix-with-secrets" ''
    set -a
    . "$CREDENTIALS_DIRECTORY/env"
    set +a
    exec "$@"
  '';

  override =
    if builtins.pathExists (dir + "/override.nix")
    then import (dir + "/override.nix") { inherit pkgs lib pkg name; app = def; }
    else { };

  unitName = "coolix-app-${name}@.service";
  dbUnits = lib.attrNames services;

  unitAttrs = lib.recursiveUpdate
    {
      Unit = {
        Description = "coolix app ${name} (%i)";
        After = [ "network.target" ] ++ dbUnits;
      } // lib.optionalAttrs (dbUnits != [ ]) { Requires = dbUnits; };
      Service = hardening // lib.optionalAttrs (hardening.DynamicUser == true) { User = user; } // {
        Type = "simple";
        ExecStart = if hasSecrets then "${withSecrets} ${exe}" else exe;
        EnvironmentFile = "${envDir}/%i.env";
        Restart = "on-failure";
        RestartSec = 2;
        StateDirectory = "coolix-apps/${name}";
        WorkingDirectory = "%S/coolix-apps/${name}";
        SyslogIdentifier = "coolix-${name}";
      } // lib.optionalAttrs hasSecrets {
        LoadCredentialEncrypted = "env:/var/lib/coolix-secrets/${name}.cred";
      };
    }
    override;
in
{
  inherit unitName;
  unit = pkgs.writeText "coolix-app-${name}-unit" (toUnit unitAttrs);
  services = lib.mapAttrs (n: text: pkgs.writeText "${lib.removeSuffix ".service" n}-unit" text) services;
  serviceNames = dbUnits;
  manifest = {
    unit = unitName;
    inherit ports;
    domains = def.domains or [ ];
    health = def.health or { };
    secrets = hasSecrets;
  };
}
