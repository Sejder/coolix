{ lib, buildGoModule }:

buildGoModule {
  pname = "coolix";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  vendorHash = "sha256-pbA/AlBz3cQYRTMnQ/qBPcinYOKokrBLNhkbRTq54gE=";
  subPackages = [ "cmd/coolix" ];

  meta = {
    description = "NixOS-native self-hosting PaaS";
    mainProgram = "coolix";
  };
}
