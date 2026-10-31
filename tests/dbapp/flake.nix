# App used by the data VM test: on start it bumps a counter in postgres and
# redis and publishes the results (and its secret) as static files.
{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }: {
    packages.x86_64-linux.default = import ./package.nix {
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
    };
  };
}
