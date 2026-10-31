# Milestone 1: add an app, deploy it, redeploy a new version, remove it,
# and check it comes back after a reboot. The VM is offline, so everything the
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
    machine.succeed("cp -rL /etc/coolix-test-app /root/app && chmod -R u+w /root/app")

    machine.succeed("coolix app add hello --source path:/root/app")
    machine.succeed("coolix app list | grep hello")
    machine.succeed("coolix deploy hello >&2")

    machine.wait_for_unit("coolix-app-hello@blue.service")
    machine.wait_for_open_port(20000)
    machine.succeed("curl -sf http://127.0.0.1:20000/ | grep -x v1")

    # Hardening is on.
    machine.succeed("systemctl show coolix-app-hello@blue -p DynamicUser | grep -x DynamicUser=yes")

    with subtest("redeploy picks up a new revision"):
        machine.succeed("echo v2 > /root/app/VERSION")
        machine.succeed("coolix deploy hello >&2")
        machine.wait_until_succeeds("curl -sf http://127.0.0.1:20000/ | grep -x v2")

    with subtest("deploys are committed to the apps repo"):
        machine.succeed("git -C /var/lib/coolix/repo log --oneline | grep 'Deploy hello'")

    with subtest("apps come back after reboot"):
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("coolix-activate-boot.service")
        machine.wait_for_open_port(20000)
        machine.succeed("curl -sf http://127.0.0.1:20000/ | grep -x v2")

    with subtest("removing an app stops it"):
        machine.succeed("coolix app rm hello")
        machine.succeed("coolix deploy >&2")
        machine.fail("systemctl is-active coolix-app-hello@blue.service")
        machine.fail("test -e /run/systemd/system/coolix-app-hello@.service")
  '';
}
