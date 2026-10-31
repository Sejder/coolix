# git push deploys: push apps (source = "push") get a bare repo on the host;
# a push to the deploy branch over SSH, as coolix@host, builds and deploys
# it. The coolix user's SSH logins can only do that.
{ self }:
{ pkgs, lib, ... }:

let
  testApp = version: import ./app/package.nix { inherit pkgs version; };
  sshKeys = import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs;
in
{
  name = "coolix-git";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ];
    services.openssh.enable = true;
    services.coolix = {
      enable = true;
      caddy.tls = "off";
      drainTime = "0s";
      ui.enable = false;
      git = {
        enable = true;
        host = "localhost";
        authorizedKeys = [ sshKeys.snakeOilPublicKey ];
      };
    };
    networking.hosts."127.0.0.1" = [ "web.test" ];

    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 4096;
    nix.settings = {
      substituters = lib.mkForce [ ];
      flake-registry = "";
    };
    environment.etc."coolix-test-app".source = ./app;
    environment.systemPackages = [ pkgs.curl pkgs.git ];

    system.extraDependencies = [
      (testApp "v1")
      (testApp "v2")
      (testApp "v3")
      pkgs.stdenvNoCC
      pkgs.bash
      pkgs.coreutils
      pkgs.path
      self.outPath
    ];
  };

  testScript = ''
    import shlex

    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("sshd.service")
    machine.wait_for_unit("caddy.service")

    ssh_opts = "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o BatchMode=yes"
    machine.succeed(
        "install -D -m 0600 ${sshKeys.snakeOilPrivateKey} /root/.ssh/declared",
        "ssh-keygen -q -t ed25519 -N ''' -C ci@test -f /root/.ssh/added",
        "git config --global user.name test && git config --global user.email test@test",
        "git config --global init.defaultBranch main",
    )

    def git(cmd, key="declared"):
        return f"cd /root/web && GIT_SSH_COMMAND='ssh {ssh_opts} -i /root/.ssh/{key}' git {cmd} 2>&1"

    def served(version):
        machine.wait_until_succeeds(f"curl -sf http://web.test/ | grep -x {version}", timeout=30)

    with subtest("a push app has nothing to deploy until something is pushed"):
        machine.succeed("coolix app add web --source push --domain web.test --health-path /healthz")
        machine.succeed("test -f /var/lib/coolix/git/web.git/HEAD")
        out = machine.fail("coolix deploy web 2>&1")
        assert "nothing pushed to main yet" in out and "git push coolix@localhost:web main" in out, out
        # Deploying everything skips it instead of failing.
        machine.succeed("coolix deploy >&2")
        machine.succeed("coolix status | grep 'web.*not deployed'")

    with subtest("git push deploys"):
        machine.succeed(
            "cp -rL /etc/coolix-test-app /root/web && chmod -R u+w /root/web",
            "cd /root/web && echo v1 > VERSION && git init -q && git add -A && git commit -qm v1",
            "cd /root/web && git remote add coolix coolix@localhost:web",
        )
        out = machine.succeed(git("push coolix main"))
        assert "deploying web" in out and "started [web]" in out, out
        served("v1")
        machine.succeed("journalctl -u 'coolix-control@*' | grep 'started \\[web\\]'")

    with subtest("only the deploy branch deploys"):
        machine.succeed("cd /root/web && git checkout -qb feature && echo v3 > VERSION && git commit -qam v3")
        out = machine.succeed(git("push coolix feature"))
        assert "only pushes to main deploy web" in out, out
        served("v1")
        machine.succeed("cd /root/web && git checkout -q main")

    with subtest("keys added with coolix ssh-key can push too"):
        machine.fail(git("push coolix main", key="added"))
        machine.succeed("coolix ssh-key add < /root/.ssh/added.pub")
        machine.succeed("coolix ssh-key list | grep ci@test")
        machine.succeed("cd /root/web && echo v2 > VERSION && git commit -qam v2")
        out = machine.succeed(git("push coolix main", key="added"))
        assert "ci@test pushed to web" in out, out
        served("v2")
        # Fetching works; the deploy branch is what's running.
        machine.succeed(git("fetch -q coolix") + " && cd /root/web && test $(git rev-parse coolix/main) = $(git rev-parse main)")
        machine.succeed("coolix ssh-key rm ci@test")
        machine.fail(git("push coolix main", key="added"))

    with subtest("a force push of an older commit deploys it"):
        machine.succeed(git("push -f coolix main~1:main"))
        served("v1")

    with subtest("the coolix user's SSH logins only do git"):
        ssh = f"ssh {ssh_opts} -i /root/.ssh/declared coolix@localhost"
        out = machine.fail(f"{ssh} 2>&1")
        assert "only accepts git push and fetch" in out, out
        for cmd in ["id", "sh -c id", "git-receive-pack '/etc'", "git-upload-archive 'web'", "git-receive-pack '../repo'"]:
            out = machine.fail(f"{ssh} {shlex.quote(cmd)} 2>&1")
            assert "uid=" not in out, out
        # Forwarding is refused: nothing answers through the tunnel.
        machine.succeed(f"{ssh} -f -N -L 9999:127.0.0.1:22")
        machine.fail("timeout 5 bash -c 'exec 3<>/dev/tcp/127.0.0.1/9999 && head -c 3 <&3 | grep SSH'")
        machine.succeed("pkill -f 'ssh .*-L 9999'")
        # Only push apps can be pushed to.
        machine.succeed("coolix app add other --source path:/root/web --domain other.test")
        out = machine.fail(git("push coolix@localhost:other main") + "; exit 1")
        assert "no push app" in out, out
        out = machine.fail(git("push coolix@localhost:nope main") + "; exit 1")
        assert "no push app" in out, out
        machine.fail("test -e /var/lib/coolix/git/nope.git")

    with subtest("removing a push app deletes its pushed code"):
        machine.succeed("coolix app rm web")
        machine.fail("test -e /var/lib/coolix/git/web.git")
  '';
}
