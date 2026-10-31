# Milestones 1-3: deploy through Caddy, zero-downtime blue/green redeploys,
# failed health checks, privilege separation, generations, rollback, reboot
# persistence and removal. The VM is offline, so everything the in-VM nix
# builds must already be in its store (extraDependencies).
{ self }:
{ pkgs, lib, ... }:

let
  testApp = version: import ./app/package.nix { inherit pkgs version; };
in
{
  name = "coolix-basic";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ];
    services.coolix = {
      enable = true;
      caddy.tls = "off";
      drainTime = "1s";
      ui.enable = false;
    };
    networking.hosts."127.0.0.1" = [ "hello.test" ];

    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 4096;
    # Keep built profiles across the reboot subtest (default is a tmpfs store).
    virtualisation.writableStoreUseTmpfs = false;
    nix.settings = {
      substituters = lib.mkForce [ ];
      flake-registry = "";
    };
    environment.etc."coolix-test-app".source = ./app;
    environment.systemPackages = [ pkgs.curl pkgs.git ];

    system.extraDependencies = [
      (testApp "v1")
      (testApp "v2")
      (testApp "broken")
      pkgs.stdenvNoCC
      pkgs.bash
      pkgs.coreutils
      pkgs.path
      self.outPath
    ];
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("caddy.service")

    # A writable copy of the app source, so we can bump its version.
    machine.succeed("cp -rL /etc/coolix-test-app /srv/app && chmod -R u+w /srv/app")

    def served(version):
        machine.wait_until_succeeds(f"curl -sf http://hello.test/ | grep -x {version}", timeout=30)

    def set_version(v):
        machine.succeed(f"echo {v} > /srv/app/VERSION")

    machine.succeed("coolix app add hello --source path:/srv/app --domain hello.test --health-path /healthz --health-timeout 5s")
    machine.succeed("coolix app list | grep hello")
    machine.succeed("coolix deploy hello >&2")
    served("v1")
    machine.succeed("systemctl is-active coolix-app-hello@blue.service")

    with subtest("hardening is on"):
        machine.succeed("systemctl show coolix-app-hello@blue -p DynamicUser | grep -x DynamicUser=yes")

    with subtest("root drops to the coolix user; only activation runs as root"):
        machine.succeed("test $(stat -c %U /var/lib/coolix/repo/.git) = coolix")
        machine.succeed("journalctl -u 'coolix-activate@*' | grep 'started \\[hello\\]'")

    with subtest("coolix user may start only valid activation units"):
        machine.fail("runuser -u coolix -- systemctl start --no-ask-password systemd-tmpfiles-clean.service")
        machine.fail("runuser -u coolix -- systemctl start --no-ask-password coolix-activate@foo.service")

    with subtest("blue/green redeploy has zero downtime"):
        machine.succeed(
            "systemd-run --unit=traffic --setenv=PATH=/run/current-system/sw/bin bash -c "
            "'while true; do curl -sf -m 2 http://hello.test/ >/dev/null && echo ok || echo fail; sleep 0.05; done > /tmp/traffic.log'"
        )
        machine.wait_until_succeeds("grep -c ok /tmp/traffic.log", timeout=30)
        set_version("v2")
        machine.succeed("runuser -u coolix -- coolix deploy hello >&2")
        served("v2")
        machine.succeed("sleep 1; systemctl stop traffic")
        traffic = machine.succeed("sort /tmp/traffic.log | uniq -c")
        print(traffic)
        assert "fail" not in traffic, f"requests failed during redeploy:\n{traffic}"
        machine.succeed("systemctl is-active coolix-app-hello@green.service")
        machine.fail("systemctl is-active coolix-app-hello@blue.service")
        machine.succeed("coolix status | grep -E '^hello +green +20001 +hello.test'")
        machine.succeed("runuser -u coolix -- git -C /var/lib/coolix/repo log --oneline | grep 'Deploy hello'")

    with subtest("unhealthy deploy keeps the old version serving"):
        set_version("broken")
        out = machine.fail("coolix deploy hello 2>&1")
        print(out)
        assert "previous generation restored" in out, out
        served("v2")
        machine.succeed("systemctl is-active coolix-app-hello@green.service")
        machine.fail("systemctl is-active coolix-app-hello@blue.service")
        machine.succeed("coolix status | grep 'run `coolix deploy` to apply'")
        out = machine.succeed("coolix generations")
        print(out)
        assert "2 (current)" in out, out

    with subtest("rollback restores the running app and the repo"):
        machine.succeed("coolix rollback >&2")
        served("v1")
        out = machine.succeed("coolix generations")
        assert "4 (current)" in out, out
        machine.succeed("runuser -u coolix -- git -C /var/lib/coolix/repo log -1 --format=%s | grep 'Rollback to generation 1'")
        machine.succeed("coolix status | grep 'up to date'")
        machine.fail("coolix rollback 4")  # already current
        machine.fail("coolix rollback 99")

    with subtest("rollback to an explicit generation"):
        machine.succeed("coolix rollback 2 >&2")
        served("v2")

    with subtest("apps come back after reboot"):
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("coolix-activate-boot.service")
        served("v2")

    with subtest("removing an app stops it and its route"):
        set_version("v2")
        machine.succeed("coolix app rm hello")
        machine.succeed("coolix deploy >&2")
        machine.fail("systemctl is-active coolix-app-hello@blue.service coolix-app-hello@green.service")
        machine.fail("test -e /run/systemd/system/coolix-app-hello@.service")
        machine.fail("test -e /run/coolix/caddy/hello.caddy")
        machine.fail("curl -sf http://hello.test/ | grep v")
  '';
}
