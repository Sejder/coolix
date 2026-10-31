# Milestones 1-2: deploy, redeploy, privilege separation, generations,
# rollback, reboot persistence and removal.
# The VM is offline, so everything the
# in-VM nix builds must already be in its store (extraDependencies).
{ self }:
{ pkgs, lib, ... }:

let
  testApp = version: import ./app/package.nix { inherit pkgs version; };
in
{
  name = "coolix-basic";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ];
    services.coolix.enable = true;

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
      pkgs.stdenvNoCC
      pkgs.bash
      pkgs.coreutils
      pkgs.path
      self.outPath
    ];
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    # A writable copy of the app source, so we can bump its version.
    machine.succeed("cp -rL /etc/coolix-test-app /srv/app && chmod -R u+w /srv/app")

    def served(version):
        machine.wait_until_succeeds(f"curl -sf http://127.0.0.1:20000/ | grep -x {version}", timeout=30)

    machine.succeed("coolix app add hello --source path:/srv/app")
    machine.succeed("coolix app list | grep hello")
    machine.succeed("coolix deploy hello >&2")
    machine.wait_for_unit("coolix-app-hello@blue.service")
    served("v1")

    with subtest("hardening is on"):
        machine.succeed("systemctl show coolix-app-hello@blue -p DynamicUser | grep -x DynamicUser=yes")

    with subtest("root drops to the coolix user; only activation runs as root"):
        machine.succeed("test $(stat -c %U /var/lib/coolix/repo/.git) = coolix")
        machine.succeed("journalctl -u 'coolix-activate@*' | grep 'started \\[hello\\]'")

    with subtest("coolix user may start only valid activation units"):
        machine.fail("runuser -u coolix -- systemctl start --no-ask-password systemd-tmpfiles-clean.service")
        machine.fail("runuser -u coolix -- systemctl start --no-ask-password coolix-activate@foo.service")

    with subtest("redeploy as the coolix user picks up a new revision"):
        machine.succeed("echo v2 > /srv/app/VERSION")
        machine.succeed("runuser -u coolix -- coolix deploy hello >&2")
        served("v2")
        machine.succeed("runuser -u coolix -- git -C /var/lib/coolix/repo log --oneline | grep 'Deploy hello'")

    with subtest("generations"):
        out = machine.succeed("coolix generations")
        print(out)
        assert "2 (current)" in out, out
        assert "hello" in out, out

    with subtest("rollback restores the running app and the repo"):
        machine.succeed("coolix rollback >&2")
        served("v1")
        out = machine.succeed("coolix generations")
        assert "3 (current)" in out, out
        machine.succeed("runuser -u coolix -- git -C /var/lib/coolix/repo log -1 --format=%s | grep 'Rollback to generation 1'")
        machine.fail("coolix rollback 3")  # already current
        machine.fail("coolix rollback 99")

    with subtest("rollback to an explicit generation"):
        machine.succeed("coolix rollback 2 >&2")
        served("v2")

    with subtest("apps come back after reboot"):
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("coolix-activate-boot.service")
        served("v2")

    with subtest("removing an app stops it"):
        machine.succeed("coolix app rm hello")
        machine.succeed("coolix deploy >&2")
        machine.fail("systemctl is-active coolix-app-hello@blue.service")
        machine.fail("test -e /run/systemd/system/coolix-app-hello@.service")
  '';
}
