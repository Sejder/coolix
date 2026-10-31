{ lib }:

{
  # Turn a coolix apps repo (apps/<name>/app.toml) into an app profile whose
  # only content is $out/apps.json: the apps' store paths and settings. It
  # references every package, so the profile's generations keep them alive.
  mkAppProfile = { inputs, root, system }:
    let
      pkgs = inputs.nixpkgs.legacyPackages.${system};
      appsDir = root + "/apps";
      entries = if builtins.pathExists appsDir then builtins.readDir appsDir else { };
      names = lib.attrNames (lib.filterAttrs
        (n: t: t == "directory" && builtins.pathExists (appsDir + "/${n}/app.toml"))
        entries);

      apps = lib.genAttrs names (name: (import ./app.nix {
        inherit pkgs lib inputs system name;
        dir = appsDir + "/${name}";
      }).manifest);

      manifest = pkgs.writeText "coolix-apps.json" (builtins.toJSON {
        version = 2;
        rev = inputs.self.rev or inputs.self.dirtyRev or "unknown";
        inherit apps;
      });

      profile = pkgs.runCommandLocal "coolix-app-profile" { } ''
        mkdir $out
        cp ${manifest} $out/apps.json
      '';
    in
    {
      packages.${system}.coolixProfile = profile;
    };
}
