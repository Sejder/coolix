{ lib, buildNpmPackage }:

buildNpmPackage {
  pname = "app";
  version = "0.1.0";
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [ ./package.json ./package-lock.json ./server.js ];
  };
  # Update with the hash nix prints whenever package-lock.json changes.
  npmDepsHash = "sha256-HM9jXWGTotWJwgjoVamKCB2L0wq0N/63e5p+2Q9LXas=";
  # The template has no dependencies yet; drop these two once you add some.
  forceEmptyCache = true;
  preInstall = "mkdir -p node_modules";
  dontNpmBuild = true;
  meta.mainProgram = "app";
}
