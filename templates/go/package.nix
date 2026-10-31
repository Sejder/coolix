{ lib, buildGoModule }:

buildGoModule {
  pname = "app";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [ ./go.mod ./main.go ];
  };
  # Set to the hash nix prints once you add dependencies.
  vendorHash = null;
  meta.mainProgram = "app";
}
