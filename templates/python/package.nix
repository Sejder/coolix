# For dependencies from uv.lock, see uv2nix (github:pyproject-nix/uv2nix).
{ lib, python3Packages }:

python3Packages.buildPythonApplication {
  pname = "app";
  version = "0.1.0";
  pyproject = true;
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [ ./pyproject.toml ./src ];
  };
  build-system = [ python3Packages.hatchling ];
  meta.mainProgram = "app";
}
