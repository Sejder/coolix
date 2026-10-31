# Milestone 4: per-app postgres + redis and encrypted secrets.
{ self }:
{ pkgs, lib, ... }:

{
  name = "coolix-data";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ];
    services.coolix = {
      enable = true;
      caddy.enable = false;
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
    environment.systemPackages = [ pkgs.curl pkgs.git ];

    system.extraDependencies = [
      (import ./dbapp/package.nix { inherit pkgs; })
      pkgs.postgresql_18
      pkgs.redis
      pkgs.stdenvNoCC
      pkgs.bash
      pkgs.coreutils
      pkgs.path
      self.outPath
    ];
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")
    machine.succeed("cp -rL /etc/coolix-dbapp /srv/dbapp")

    def get(path):
        return machine.succeed(f"curl -sf http://127.0.0.1:20000/{path} || curl -sf http://127.0.0.1:20001/{path}").strip()

    machine.succeed("coolix app add db --source path:/srv/dbapp --db postgres --db redis")
    machine.succeed("echo -n 's3cr3t value' | coolix secret set db API_TOKEN")
    machine.succeed("coolix secret list db | grep 'API_TOKEN (changed, not deployed)'")
    machine.succeed("coolix deploy db >&2")
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:20000/pg", timeout=60)

    with subtest("databases are wired up"):
        assert get("pg") == "1", get("pg")
        assert get("redis") == "1", get("redis")
        machine.succeed("systemctl is-active coolix-db-db-postgres.service coolix-db-db-redis.service")
        assert get("user") == "coolix-app-db", get("user")

    with subtest("database sockets are private to the app"):
        machine.fail("runuser -u nobody -- test -r /run/coolix-db/db-postgres/.s.PGSQL.5432")
        machine.fail("stat -c %a /run/coolix-db/db-postgres | grep -v 700")

    with subtest("secrets reach the app but not git, the store or disk in plaintext"):
        assert get("secret") == "s3cr3t value", get("secret")
        machine.succeed("coolix secret list db | grep -x API_TOKEN")
        machine.succeed("test -e /var/lib/coolix-secrets/db.cred")
        machine.fail("test -e /var/lib/coolix/secrets-pending/db.json")
        machine.fail("grep -r 's3cr3t' /var/lib/coolix /var/lib/coolix-secrets")
        machine.fail("grep -rl 's3cr3t' $(cat /nix/var/nix/profiles/coolix/apps/units/*.service | grep -o '/nix/store/[^/ ]*' | sort -u)")

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

  '';
}
