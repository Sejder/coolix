{
  description = "Coolix: a NixOS-native self-hosting PaaS";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      lib = import ./nix/lib { inherit (nixpkgs) lib; };

      packages = forAllSystems (pkgs: {
        coolix = pkgs.callPackage ./nix/package.nix { };
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.coolix;
      });

      nixosModules.default = import ./nix/module.nix { inherit self; };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell { packages = with pkgs; [ go gopls gotools ]; };
      });

      checks = forAllSystems (pkgs: {
        inherit (self.packages.${pkgs.stdenv.hostPlatform.system}) coolix;
      } // nixpkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
        vm-basic = pkgs.testers.runNixOSTest (import ./tests/basic.nix { inherit self; });
      });
    };
}
