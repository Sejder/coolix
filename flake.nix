{
  description = "Coolix: a NixOS-native self-hosting PaaS";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      templateNames = [ "go" "node" "python" "rust" ];
    in
    {
      lib = import ./nix/lib { inherit lib; };

      packages = forAllSystems (pkgs: {
        coolix = pkgs.callPackage ./nix/package.nix { };
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.coolix;
      });

      nixosModules.default = import ./nix/module.nix { inherit self; };

      templates = lib.genAttrs templateNames (name: {
        path = ./templates/${name};
        description = "A ${name} web app deployable with coolix";
      }) // {
        default = self.templates.go;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell { packages = with pkgs; [ go_1_27 gopls gotools ]; };
      });

      checks = forAllSystems (pkgs:
        let
          # Each template must build, serve / and /healthz on $PORT.
          templateCheck = name:
            let app = pkgs.callPackage ./templates/${name}/package.nix { };
            in pkgs.runCommand "template-${name}-check" { nativeBuildInputs = [ pkgs.curl ]; } ''
              export PORT=18080
              ${lib.getExe app} &
              for _ in $(seq 1 100); do curl -sf localhost:$PORT/healthz && break; sleep 0.1; done
              curl -sf localhost:$PORT/ | grep 'Hello from coolix'
              kill %1
              touch $out
            '';
        in
        {
          inherit (self.packages.${pkgs.stdenv.hostPlatform.system}) coolix;
        }
        // lib.listToAttrs (map (n: lib.nameValuePair "template-${n}" (templateCheck n)) templateNames)
        // lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
          vm-basic = pkgs.testers.runNixOSTest (import ./tests/basic.nix { inherit self; });
          vm-data = pkgs.testers.runNixOSTest (import ./tests/data.nix { inherit self; });
          vm-ui = pkgs.testers.runNixOSTest (import ./tests/ui.nix { inherit self; });
          vm-git = pkgs.testers.runNixOSTest (import ./tests/git.nix { inherit self; });
        });
    };
}
