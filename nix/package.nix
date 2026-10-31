{ lib, buildGo127Module }:

buildGo127Module {
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

  vendorHash = "sha256-d18zKAbX+DJcGY9q7sOFsW6enQf0neA93wYOSZJFNf4=";
  subPackages = [ "cmd/coolix" ];

  meta = {
    description = "NixOS-native self-hosting PaaS";
    mainProgram = "coolix";
  };
}
