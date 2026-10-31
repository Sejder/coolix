# Minimal app used by the VM tests. Serves the contents of ./VERSION on $PORT.
{
  # Overridden by coolix (follow_nixpkgs) so the VM never fetches it.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }: {
    packages.x86_64-linux.default = import ./package.nix {
      pkgs = nixpkgs.legacyPackages.x86_64-linux;
      version = nixpkgs.lib.fileContents ./VERSION;
    };
  };
}
