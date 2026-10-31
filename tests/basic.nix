# Milestones 1-3: deploy through Caddy, zero-downtime blue/green redeploys,
# failed health checks, privilege separation, generations, rollback, reboot
# persistence and removal. The VM is offline, so everything the in-VM nix
# builds must already be in its store (extraDependencies).
{ self }:
{ pkgs, lib, ... }:

let
  testApp = version: import ./app/package.nix { inherit pkgs version; };

  # A profile asking for a unit outside the sandbox (root), as the coolix
  # user could build one by hand.
  # What coolix built before root rendered units (apps.json version 1).
  v1Profile = pkgs.runCommandLocal "coolix-app-profile" { } ''
    mkdir -p $out/units
    echo '{"version":1,"rev":"old","apps":{},"services":[]}' > $out/apps.json
  '';

  evilManifest = pkgs.writeText "evil-apps.json" (builtins.toJSON {
    version = 2;
    rev = "evil";
    apps.evil = {
      exe = "${pkgs.bash}/bin/bash";
      port_base = 20010;
      hardening.DynamicUser = false;
      health = { };
      resources = { };
      backup = { };
    };
  });
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
    environment.etc."coolix-evil.nix".text = ''
      with import ${pkgs.path} { }; runCommandLocal "coolix-app-profile" { json = builtins.readFile ${evilManifest}; passAsFile = [ "json" ]; } "mkdir $out; cp $jsonPath $out/apps.json"
    '';
    environment.etc."coolix-override.nix".text = ''{ ... }: { Service.User = "root"; }'';
    environment.systemPackages = [ pkgs.curl pkgs.git pkgs.socat ];

    system.extraDependencies = [
      (testApp "v1")
      (testApp "v2")
      (testApp "broken")
      evilManifest
      v1Profile
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
        machine.succeed("readlink /run/systemd/system/coolix-app-hello@.service | grep ^/run/coolix/units/")
        machine.succeed("test $(stat -c %U /run/coolix/units) = root")

    def control(request, user="coolix"):
        # Talk to coolix-control like the CLI and UI do.
        return machine.succeed(f"echo '{request}' | runuser -u {user} -- socat -t 30 - UNIX-CONNECT:/run/coolix/control.sock")

    with subtest("root drops to the coolix user; only coolix-control runs as root"):
        machine.succeed("test $(stat -c %U /var/lib/coolix/repo/.git) = coolix")
        machine.succeed("journalctl -u 'coolix-control@*' | grep 'started \\[hello\\]'")

    with subtest("the coolix user can't start or create units"):
        for cmd in [
            "systemctl start --no-ask-password systemd-tmpfiles-clean.service",
            "systemctl start --no-ask-password coolix-app-hello@green.service",
            "systemctl start --no-ask-password coolix-backup-hello.service",
            # Transient units: what a "start units named like X" polkit rule allows.
            "systemd-run --no-ask-password --wait --unit=coolix-backup-pwn touch /root/pwned",
            "systemd-run --no-ask-password --wait --unit=coolix-restore-x@20260101T000000Z.dump touch /root/pwned",
            "systemd-run --no-ask-password --wait --unit=coolix-control@pwn touch /root/pwned",
        ]:
            machine.fail(f"runuser -u coolix -- {cmd}")
        machine.fail("test -e /root/pwned")
        # The control socket only takes its fixed requests, and only from coolix.
        machine.succeed("echo 'start sshd' | runuser -u coolix -- socat - UNIX-CONNECT:/run/coolix/control.sock | grep 'bad request'")
        machine.fail("echo 'backup hello' | runuser -u nobody -- socat - UNIX-CONNECT:/run/coolix/control.sock")

    with subtest("the coolix user can't get root through a crafted profile"):
        # Built by the coolix user like any profile, but asking for a root unit.
        evil = machine.succeed("runuser -u coolix -- nix-build --no-out-link /etc/coolix-evil.nix").strip()
        out = control(f"activate {evil.removeprefix('/nix/store/')}")
        assert "refusing to activate" in out and "DynamicUser" in out, out
        machine.fail("test -e /run/systemd/system/coolix-app-evil@.service")
        machine.fail("systemctl is-active coolix-app-evil@blue.service")
        out = machine.succeed("coolix generations")
        assert len(out.strip().splitlines()) == 2, out  # header + generation 1
        served("v1")

    with subtest("apps can't reach Caddy's admin API"):
        machine.succeed("test -S /run/caddy/admin.sock")
        machine.fail("curl -s --max-time 5 http://localhost:2019/config/")
        machine.fail("systemd-run --quiet --wait --pipe -p DynamicUser=yes ${pkgs.curl}/bin/curl -s --unix-socket /run/caddy/admin.sock http://x/config/")

    with subtest("override.nix is refused"):
        machine.succeed("runuser -u coolix -- cp /etc/coolix-override.nix /var/lib/coolix/repo/apps/hello/override.nix")
        out = machine.fail("coolix deploy hello 2>&1")
        assert "override.nix is no longer supported" in out, out
        machine.succeed("rm /var/lib/coolix/repo/apps/hello/override.nix")

    with subtest("the coolix user reads app logs through coolix-query, not the journal"):
        machine.fail("id -nG coolix | grep -w systemd-journal")
        machine.succeed("journalctl -u coolix-app-hello@blue | grep 'GET /healthz'")  # it's there...
        out = machine.succeed("runuser -u coolix -- journalctl -q -u coolix-app-hello@blue 2>&1 || true")
        assert "GET /healthz" not in out, out  # ...but not for coolix directly
        machine.succeed("runuser -u coolix -- coolix logs hello | grep 'GET /healthz'")
        machine.succeed("runuser -u coolix -- coolix logs -n 5 hello | grep 'GET /healthz'")
        machine.fail("runuser -u coolix -- coolix logs hello -n 99999")
        machine.succeed("echo 'unit sshd.service' | runuser -u coolix -- socat - UNIX-CONNECT:/run/coolix/query.sock | grep 'bad request'")
        machine.fail("echo 'logs hello 10 0' | runuser -u nobody -- socat - UNIX-CONNECT:/run/coolix/query.sock")

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
        # The failed generation was deleted, so this is generation 3.
        assert "3 (current)" in out, out
        machine.succeed("runuser -u coolix -- git -C /var/lib/coolix/repo log -1 --format=%s | grep 'Rollback to generation 1'")
        machine.succeed("coolix status | grep 'up to date'")
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

    with subtest("removing an app stops it and its route"):
        set_version("v2")
        machine.succeed("coolix app rm hello")
        machine.succeed("coolix deploy >&2")
        machine.fail("systemctl is-active coolix-app-hello@blue.service coolix-app-hello@green.service")
        machine.fail("test -e /run/systemd/system/coolix-app-hello@.service")
        machine.fail("test -e /run/coolix/caddy/hello.caddy")
        out = machine.succeed("curl -s --max-time 5 http://hello.test/ || true")
        assert "v2" not in out, out

    with subtest("a profile from an older coolix doesn't break boot"):
        machine.succeed("nix-env --profile /nix/var/nix/profiles/coolix/apps --set ${v1Profile}")
        machine.succeed("systemctl restart coolix-activate-boot.service")
        machine.succeed("journalctl -u coolix-activate-boot | grep 'built by an older coolix'")
        machine.succeed("coolix status | grep 'older coolix'")
        # A deploy rebuilds it.
        machine.succeed("coolix app add hello --source path:/srv/app --domain hello.test --health-path /healthz --health-timeout 5s")
        machine.succeed("coolix deploy >&2")
        served("v2")

    with subtest("a failed deploy on top of an older profile keeps the new generation"):
        machine.succeed("nix-env --profile /nix/var/nix/profiles/coolix/apps --set ${v1Profile}")
        set_version("broken")
        out = machine.fail("coolix deploy hello 2>&1")
        assert "can't be re-activated" in out, out
        served("v2")  # the healthy old slot keeps serving
        machine.succeed("systemctl restart coolix-activate-boot.service")
  '';
}
