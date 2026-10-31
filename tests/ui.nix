# Milestone 5: the web UI end to end in a real browser (headless Chromium via
# selenium), including a deploy started from the UI. Also produces the README
# screenshots: nix build .#checks.x86_64-linux.vm-ui && cp result/screenshots/*.png docs/screenshots/
{ self }:
{ pkgs, lib, ... }:

let
  testApp = version: import ./app/package.nix { inherit pkgs version; };
  python = pkgs.python3.withPackages (p: [ p.selenium ]);
in
{
  name = "coolix-ui";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.default ];
    services.coolix = {
      enable = true;
      caddy.tls = "off";
      drainTime = "0s";
      # No adminPasswordFile: the account is created on the setup page.
      ui.domain = "coolix.test";
    };
    networking.hosts."127.0.0.1" = [ "coolix.test" "hello.test" "www.hello.test" "docs.test" ];

    virtualisation.memorySize = 4096;
    virtualisation.diskSize = 6144;
    nix.settings = {
      substituters = lib.mkForce [ ];
      flake-registry = "";
    };
    environment.etc."coolix-test-app".source = ./app;
    environment.systemPackages = [ pkgs.curl pkgs.git pkgs.socat pkgs.openssl pkgs.chromium pkgs.chromedriver python ];
    fonts.packages = [ pkgs.inter pkgs.jetbrains-mono ];

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
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("coolix.service")
    machine.wait_for_unit("caddy.service")
    machine.wait_until_succeeds("curl -sf http://coolix.test/healthz")

    with subtest("a fresh install only offers setup, which needs a code from the host"):
        machine.succeed("curl -s -o /dev/null -w '%{redirect_url}' http://coolix.test/ | grep /setup")
        machine.succeed("test $(curl -s -o /dev/null -w '%{http_code}' -d code=AAAA-BBBB-CCCC-DDDD http://coolix.test/setup) = 401")
        code = machine.succeed("coolix setup-code | awk '/^Setup code:/ {print $3}'").strip()
        assert len(code) == 19, code

    # Some state worth looking at: two deployed apps, one undeployed change.
    machine.succeed("cp -rL /etc/coolix-test-app /srv/hello && cp -rL /etc/coolix-test-app /srv/docs && chmod -R u+w /srv")
    machine.succeed("coolix app add docs --source path:/srv/docs --domain docs.test --health-path /healthz")
    machine.succeed("coolix app add hello --source path:/srv/hello --domain hello.test --domain www.hello.test --health-path /healthz --env LOG_LEVEL=info")
    machine.succeed("echo -n x | coolix secret set hello SESSION_SECRET")
    machine.succeed("coolix deploy >&2")
    machine.succeed("echo v2 > /srv/hello/VERSION")
    machine.succeed("coolix app set docs --env THEME=dark")

    # The UI listens on a unix socket only Caddy and root can open. Selenium
    # reaches it on localhost:8090, like an admin through an SSH tunnel
    # (passkeys need localhost or HTTPS).
    machine.succeed("systemd-run --unit=uitunnel ${pkgs.socat}/bin/socat TCP-LISTEN:8090,bind=127.0.0.1,fork,reuseaddr UNIX-CONNECT:/run/coolix/ui.sock")
    machine.wait_for_open_port(8090)

    with subtest("apps can't reach the UI except through Caddy"):
        machine.succeed("test $(stat -c %G /run/coolix/ui.sock) = caddy")
        machine.fail("systemd-run --quiet --wait --pipe -p DynamicUser=yes ${pkgs.curl}/bin/curl -sf --unix-socket /run/coolix/ui.sock http://x/healthz")

    with subtest("deploy from the browser and take screenshots"):
        machine.succeed("mkdir -p /tmp/screenshots")
        machine.succeed(
            f"python3 ${./screenshots.py} /tmp/screenshots {code} "
            "${pkgs.chromium}/bin/chromium ${pkgs.chromedriver}/bin/chromedriver >&2"
        )
        machine.succeed("curl -sf http://hello.test/ | grep -x v2")
        machine.copy_from_machine("/tmp/screenshots")

    with subtest("a signed push webhook deploys the app"):
        machine.succeed("echo v3 > /srv/hello/VERSION")
        token = machine.succeed("coolix webhook enable hello | awk '/^Secret:/ {print $2}'").strip()
        body = '{"ref":"refs/heads/main"}'
        sig = machine.succeed(f"printf %s '{body}' | openssl dgst -sha256 -hmac {token} -r | cut -d' ' -f1").strip()
        hook = "curl -sf -X POST -H 'X-GitHub-Event: push' -H 'Content-Type: application/json'"
        machine.fail(f"{hook} -H 'X-Hub-Signature-256: sha256=00' -d '{body}' http://coolix.test/hooks/hello")
        out = machine.succeed(f"{hook} -H 'X-Hub-Signature-256: sha256={sig}' -d '{body}' http://coolix.test/hooks/hello")
        assert "deploying" in out, out
        machine.wait_until_succeeds("curl -sf http://hello.test/ | grep -x v3", timeout=120)

    with subtest("UI service runs unprivileged"):
        machine.succeed("test $(systemctl show coolix -p User --value) = coolix")
  '';
}
