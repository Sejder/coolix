# Per-app postgres + redis, encrypted secrets, backups and restore, and the
# app settings that end up in root-rendered units.
{ self }:
{ pkgs, lib, ... }:

{
  name = "coolix-data";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ];
    services.coolix = {
      enable = true;
      caddy.enable = false;
      # Custom data locations (e.g. a bigger disk).
      dataDir = "/srv/coolix-data";
      secretsDir = "/srv/coolix-secrets";
      drainTime = "0s";
      ui.enable = false;
    };

    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 4096;
    nix.settings = {
      substituters = lib.mkForce [ ];
      flake-registry = "";
    };
    environment.etc."coolix-dbapp".source = ./dbapp;
    environment.systemPackages = [ pkgs.curl pkgs.git pkgs.socat ];

    system.extraDependencies = [
      (import ./dbapp/package.nix { inherit pkgs; })
      pkgs.postgresql_18
      pkgs.redis
      pkgs.hello
      pkgs.stdenvNoCC
      pkgs.bash
      pkgs.coreutils
      pkgs.path
      self.outPath
    ];
  };

  testScript = ''
    import re

    machine.wait_for_unit("multi-user.target")
    machine.succeed("cp -rL /etc/coolix-dbapp /srv/dbapp")

    def get(path):
        return machine.succeed(f"curl -sf http://127.0.0.1:20000/{path} || curl -sf http://127.0.0.1:20001/{path}").strip()

    def boots():
        return machine.succeed(
            "systemd-run --quiet --wait --pipe -p DynamicUser=yes -p User=coolix-app-db "
            "${pkgs.postgresql_18}/bin/psql -h /run/coolix-db/db-postgres -d app -qtAc 'SELECT count(*) FROM boots'"
        ).strip()

    machine.succeed(
        "coolix app add db --source path:/srv/dbapp --db postgres --db redis "
        "--pre-start bin/dbapp-migrate --path hello --memory-max 256M "
        "--env 'WEIRD=a \"b\" 100% $HOME \\ end'"
    )
    machine.succeed("echo -n 's3cr3t value' | coolix secret set db API_TOKEN")
    machine.succeed("coolix secret list db | grep 'API_TOKEN (changed, not deployed)'")
    machine.succeed("coolix deploy db >&2")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:20000/pg", timeout=60)

    with subtest("databases are wired up"):
        assert get("pg") == "1", get("pg")
        assert get("redis") == "1", get("redis")
        machine.succeed("systemctl is-active coolix-db-db-postgres.service coolix-db-db-redis.service")
        assert get("user") == "coolix-app-db", get("user")

    with subtest("pre-start, runtime packages, limits and environment quoting"):
        assert get("migrated") == "yes", get("migrated")
        assert get("hello") == "Hello, world!", get("hello")
        machine.succeed("systemctl show coolix-app-db@blue -p MemoryMax | grep -x MemoryMax=268435456")
        assert get("weird") == 'a "b" 100% $HOME \\ end', get("weird")

    with subtest("data lives in dataDir, secrets in secretsDir"):
        machine.succeed("test -s /srv/coolix-data/db/db-postgres/PG_VERSION")
        machine.succeed("test -e /srv/coolix-data/apps/db/migrated")
        machine.succeed("findmnt /var/lib/private/coolix-db")
        machine.succeed("test -e /srv/coolix-secrets/db.cred")
        machine.succeed("test $(stat -c %a /srv/coolix-data) = 700")
        machine.fail("runuser -u nobody -- ls /srv/coolix-data/db")

    with subtest("database sockets are private to the app"):
        machine.succeed("test -S /run/coolix-db/db-postgres/.s.PGSQL.5432")
        machine.fail("runuser -u nobody -- test -r /run/coolix-db/db-postgres/.s.PGSQL.5432")
        machine.succeed("test $(stat -c %a /run/coolix-db/db-postgres) = 700")
        machine.succeed("systemctl show coolix-db-db-postgres -p PrivateNetwork | grep -x PrivateNetwork=yes")

    with subtest("apps can only listen on their own ports"):
        assert get("squat") == "denied", get("squat")

    with subtest("apps can't reach the nix daemon"):
        machine.succeed("test -S /nix/var/nix/daemon-socket/socket")
        assert get("nixd") == "hidden", get("nixd")

    with subtest("secrets reach the app but not git, the store or disk in plaintext"):
        assert get("secret") == "s3cr3t value", get("secret")
        machine.succeed("coolix secret list db | grep -x API_TOKEN")
        machine.succeed("test -e /srv/coolix-secrets/db.cred")
        machine.fail("test -e /var/lib/coolix/secrets-pending/db.json")
        machine.fail("grep -r 's3cr3t' /var/lib/coolix /srv/coolix-secrets /srv/coolix-data /run/coolix")
        machine.fail("grep -rl 's3cr3t' $(grep -o '/nix/store/[^/\" ]*' /nix/var/nix/profiles/coolix/apps/apps.json | sort -u)")

    with subtest("changing a secret restarts the app into a new slot; data persists"):
        machine.succeed("echo -n rotated | coolix secret set db API_TOKEN")
        machine.succeed("coolix deploy db >&2")
        machine.wait_until_succeeds("systemctl is-active coolix-app-db@green.service")
        assert get("secret") == "rotated", get("secret")
        assert get("pg") == "2", get("pg")
        assert get("redis") == "2", get("redis")

    with subtest("unsetting a secret"):
        machine.succeed("coolix secret unset db API_TOKEN")
        machine.succeed("coolix deploy db >&2")
        machine.wait_until_succeeds("systemctl is-active coolix-app-db@blue.service")
        assert get("secret") == "unset", get("secret")

    with subtest("backups and restore"):
        assert boots() == "3", boots()
        machine.succeed("runuser -u coolix -- coolix db backup db >&2")
        out = machine.succeed("runuser -u coolix -- coolix db backups db")
        print(out)
        backup = re.findall(r"\d{8}T\d{6}Z\.dump", out)[0]
        # Dumps are private to the app's user.
        machine.fail(f"runuser -u coolix -- cat /var/lib/private/coolix-backups/db/{backup}")
        machine.succeed(f"test -s /srv/coolix-data/backups/db/{backup}")
        machine.succeed(
            "systemd-run --quiet --wait --pipe -p DynamicUser=yes -p User=coolix-app-db "
            "${pkgs.postgresql_18}/bin/psql -h /run/coolix-db/db-postgres -d app -qtAc 'INSERT INTO boots DEFAULT VALUES'"
        )
        assert boots() == "4", boots()
        machine.fail("runuser -u coolix -- coolix db restore db 20000101T000000Z.dump")
        machine.succeed("echo 'restore db ../../x' | runuser -u coolix -- socat - UNIX-CONNECT:/run/coolix/control.sock | grep 'bad request'")
        machine.fail("runuser -u coolix -- systemctl start --no-ask-password coolix-restore-db@20000101T000000Z.dump.service")
        machine.succeed(f"runuser -u coolix -- coolix db restore db {backup} >&2")
        assert boots() == "3", boots()

    with subtest("scheduled backups keep the newest ones"):
        machine.fail("coolix app set db --backup-schedule dailyx")  # systemd-analyze rejects it
        machine.succeed("coolix app set db --backup-schedule daily --backup-keep 2")
        machine.succeed("coolix deploy db >&2")
        machine.succeed("systemctl is-active coolix-backup-db.timer")
        assert boots() == "3", "unchanged app must not restart"
        for _ in range(2):
            machine.succeed("sleep 1; runuser -u coolix -- coolix db backup db >&2")
        out = machine.succeed("runuser -u coolix -- coolix db backups db")
        assert len(re.findall(r"\.dump", out)) == 2, out
        assert backup not in out, out
  '';
}
