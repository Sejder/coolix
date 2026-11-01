# git push deploys: push apps (source = "push") get a bare repo on the host;
# a push to the deploy branch over SSH, as coolix@host, builds and deploys
# it. The coolix user's SSH logins can only do that. Also previews: of
# pushed branches, of pull requests (webhooks) and by hand.
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
      ui.listen = "127.0.0.1:8090";
      previews.domain = "test";
      git = {
        enable = true;
        host = "localhost";
        authorizedKeys = [ sshKeys.snakeOilPublicKey ];
      };
    };
    networking.hosts."127.0.0.1" = [ "web.test" "web-feature.test" "hook.test" "hook-pr1.test" "hook-other.test" ];

    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 4096;
    nix.settings = {
      substituters = lib.mkForce [ ];
      flake-registry = "";
    };
    environment.etc."coolix-test-app".source = ./app;
    environment.systemPackages = [ pkgs.curl pkgs.git pkgs.openssl ];

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
        "git config --global safe.directory '*'",
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
        machine.succeed("coolix app rm other")

    with subtest("pushed branches get previews"):
        machine.succeed("coolix app set web --preview on")
        machine.succeed("cd /root/web && git checkout -q feature && echo feature1 > VERSION && git commit -qam feature1")
        out = machine.succeed(git("push coolix feature"))
        assert "preview of feature: web-feature.test" in out, out
        machine.wait_until_succeeds("curl -sf http://web-feature.test/ | grep -x feature1", timeout=30)
        served("v1")
        machine.succeed("coolix preview list web | grep web-feature")
        machine.succeed("test -d /var/lib/private/coolix-apps/web-feature")
        # Deleting the branch removes the preview and its data.
        out = machine.succeed(git("push coolix :feature"))
        assert "removing preview web-feature" in out, out
        machine.wait_until_fails("curl -sf http://web-feature.test/ | grep -x feature1", timeout=30)
        machine.fail("test -e /var/lib/private/coolix-apps/web-feature")
        machine.fail("test -e /var/lib/coolix/repo/apps/web-feature")
        machine.succeed("journalctl -u 'coolix-control@*' | grep 'deleted /var/lib/private/coolix-apps/web-feature'")
        served("v1")
        machine.succeed("cd /root/web && git checkout -q main")

    def commit(branch, version):
        machine.succeed(f"cd /srv/hook && git checkout -q -B {branch} main && echo {version} > VERSION && git commit -qam {version}")
        return machine.succeed(f"git -C /srv/hook rev-parse {branch}").strip()

    def pr_hook(action, number, branch, sha, head_repo="me/hook"):
        body = (f'{{"action":"{action}","number":{number},"pull_request":{{"head":{{"ref":"{branch}","sha":"{sha}",'
                f'"repo":{{"full_name":"{head_repo}"}}}},"base":{{"repo":{{"full_name":"me/hook"}}}}}}}}')
        sig = machine.succeed(f"printf %s '{body}' | openssl dgst -sha256 -hmac {token} -r | cut -d' ' -f1").strip()
        return machine.succeed(
            f"curl -sf -X POST -H 'X-GitHub-Event: pull_request' -H 'Content-Type: application/json' "
            f"-H 'X-Hub-Signature-256: sha256={sig}' -d '{body}' http://127.0.0.1:8090/hooks/hook"
        )

    with subtest("pull requests get previews through the webhook"):
        machine.succeed(
            "cp -rL /etc/coolix-test-app /srv/hook && chmod -R u+w /srv/hook",
            "cd /srv/hook && echo hook1 > VERSION && git init -q && git add -A && git commit -qm hook1",
            "chown -R coolix:coolix /srv/hook",
        )
        machine.succeed("coolix app add hook --source git+file:///srv/hook --domain hook.test --health-path /healthz --preview on")
        machine.succeed("echo -n s3cret | coolix secret set hook KEY")
        machine.succeed("coolix deploy hook >&2")
        machine.wait_until_succeeds("curl -sf http://hook.test/ | grep -x hook1", timeout=30)
        token = machine.succeed("coolix webhook enable hook | awk '/^Secret:/ {print $2}'").strip()

        sha = commit("prb", "pr1a")
        out = pr_hook("opened", 1, "prb", sha)
        assert "deploying" in out and "hook-pr1" in out, out
        machine.wait_until_succeeds("curl -sf http://hook-pr1.test/ | grep -x pr1a", timeout=120)
        # Previews don't get the app's secrets unless it opts in.
        machine.fail("systemctl cat coolix-app-hook-pr1@.service | grep LoadCredentialEncrypted")

        sha = commit("prb", "pr1b")
        pr_hook("synchronize", 1, "prb", sha)
        machine.wait_until_succeeds("curl -sf http://hook-pr1.test/ | grep -x pr1b", timeout=120)

        out = pr_hook("opened", 2, "evil", sha, head_repo="stranger/hook")
        assert "forks are never deployed" in out, out

        out = pr_hook("closed", 1, "prb", sha)
        assert "removing" in out, out
        machine.wait_until_fails("curl -sf http://hook-pr1.test/ | grep -x pr1b", timeout=120)
        machine.wait_until_fails("test -e /var/lib/coolix/repo/apps/hook-pr1", timeout=60)
        machine.wait_until_succeeds("curl -sf http://hook.test/ | grep -x hook1", timeout=30)

    with subtest("previews by hand, with the app's secrets when it opts in"):
        commit("other", "other1")
        machine.succeed("coolix app set hook --preview-secrets on")
        machine.succeed("coolix preview deploy hook other >&2")
        machine.wait_until_succeeds("curl -sf http://hook-other.test/ | grep -x other1", timeout=30)
        machine.succeed("systemctl cat coolix-app-hook-other@.service | grep 'LoadCredentialEncrypted=env:/var/lib/coolix-secrets/hook.cred'")
        machine.succeed("coolix preview rm hook hook-other >&2")
        machine.wait_until_fails("curl -sf http://hook-other.test/ | grep -x other1", timeout=30)

    with subtest("removing a push app deletes its pushed code"):
        machine.succeed("coolix app rm web")
        machine.fail("test -e /var/lib/coolix/git/web.git")
  '';
}
