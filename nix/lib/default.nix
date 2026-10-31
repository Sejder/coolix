{ lib }:

{
  # Turn a coolix apps repo (apps/<name>/app.toml) into an app profile:
  #   $out/units/coolix-app-<name>@.service   templated unit, instance = slot (blue|green)
  #   $out/apps.json                           manifest read by the coolix activator
  mkAppProfile = { inputs, root, system }:
    let
      pkgs = inputs.nixpkgs.legacyPackages.${system};
      appsDir = root + "/apps";
      entries = if builtins.pathExists appsDir then builtins.readDir appsDir else { };
      names = lib.attrNames (lib.filterAttrs
        (n: t: t == "directory" && builtins.pathExists (appsDir + "/${n}/app.toml"))
        entries);

      apps = lib.genAttrs names (name: import ./app.nix {
        inherit pkgs lib inputs system name;
        dir = appsDir + "/${name}";
      });

      manifest = pkgs.writeText "coolix-apps.json" (builtins.toJSON {
        version = 1;
        rev = inputs.self.rev or inputs.self.dirtyRev or "unknown";
        apps = lib.mapAttrs (_: app: app.manifest) apps;
      });

      profile = pkgs.runCommandLocal "coolix-app-profile" { } ''
        mkdir -p $out/units
        ${lib.concatMapStrings (app: ''
          cp ${app.unit} $out/units/${app.unitName}
        '') (lib.attrValues apps)}
        cp ${manifest} $out/apps.json
      '';
    in
    {
      packages.${system}.coolixProfile = profile;
    };
}
