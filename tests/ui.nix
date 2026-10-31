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
      ui = {
        domain = "coolix.test";
        adminPasswordFile = pkgs.writeText "pw" "hunter2"; # test only: never put real passwords in the store
      };
    };
    networking.hosts."127.0.0.1" = [ "coolix.test" "hello.test" "www.hello.test" "docs.test" ];

    virtualisation.memorySize = 4096;
    virtualisation.diskSize = 6144;
    nix.settings = {
      substituters = lib.mkForce [ ];
      flake-registry = "";
    };
    environment.etc."coolix-test-app".source = ./app;
    environment.systemPackages = [ pkgs.curl pkgs.git pkgs.chromium pkgs.chromedriver python ];
    fonts.packages = [ pkgs.inter pkgs.jetbrains-mono ];

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
    machine.wait_for_unit("coolix.service")
    machine.wait_for_unit("caddy.service")
    machine.wait_until_succeeds("curl -sf http://coolix.test/healthz")

    with subtest("login is required"):
        machine.succeed("curl -s -o /dev/null -w '%{redirect_url}' http://coolix.test/ | grep /login")
        machine.succeed("test $(curl -s -o /dev/null -w '%{http_code}' -d password=nope http://coolix.test/login) = 401")

    # Some state worth looking at: two deployed apps, one undeployed change.
    machine.succeed("cp -rL /etc/coolix-test-app /srv/hello && cp -rL /etc/coolix-test-app /srv/docs && chmod -R u+w /srv")
    machine.succeed("coolix app add docs --source path:/srv/docs --domain docs.test --health-path /healthz")
    machine.succeed("coolix app add hello --source path:/srv/hello --domain hello.test --domain www.hello.test --health-path /healthz --env LOG_LEVEL=info")
    machine.succeed("echo -n x | coolix secret set hello SESSION_SECRET")
    machine.succeed("coolix deploy >&2")
    machine.succeed("echo v2 > /srv/hello/VERSION")
    machine.succeed("coolix app set docs --env THEME=dark")

    with subtest("deploy from the browser and take screenshots"):
        machine.succeed("mkdir -p /tmp/screenshots")
        machine.succeed(
            "python3 ${./screenshots.py} /tmp/screenshots "
            "${pkgs.chromium}/bin/chromium ${pkgs.chromedriver}/bin/chromedriver >&2"
        )
        machine.succeed("curl -sf http://hello.test/ | grep -x v2")
        machine.copy_from_machine("/tmp/screenshots")

    with subtest("UI service runs unprivileged"):
        machine.succeed("test $(systemctl show coolix -p User --value) = coolix")
  '';
}
