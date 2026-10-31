# One app of the profile: evaluates apps/<name>/app.toml into the data the
# root activator needs (see internal/units). No unit files are generated
# here: whoever controls the apps repo controls this evaluation, so root
# renders and sandboxes the units itself.
{ pkgs, lib, inputs, system, name, dir }:

let
  def = builtins.fromTOML (builtins.readFile (dir + "/app.toml"));

  input = inputs."app-${name}" or (throw
    "coolix: flake input app-${name} is missing; run `coolix deploy` to regenerate flake.nix");
  pkg = input.packages.${system}.${def.package or "default"};
  exe = if def ? command then "${pkg}/${def.command}" else lib.getExe pkg;

  dbPackage = db:
    if db.kind == "postgres" then pkgs."postgresql_${db.version or "18"}"
    else if db.kind == "redis" then pkgs.redis
    else throw "coolix: app ${name}: unknown database kind ${db.kind}";

  pathPackage = attr: lib.getBin (lib.attrByPath (lib.splitString "." attr)
    (throw "coolix: app ${name}: path entry ${attr} is not a nixpkgs attribute")
    pkgs);

  manifest = {
    inherit exe;
    port_base = def.port_base;
    domains = def.domains or [ ];
    health = def.health or { };
    env = lib.mapAttrs (_: toString) (def.env or { });
    secrets = (def.secrets or [ ]) != [ ];
    path = map (p: "${pathPackage p}") (def.path or [ ]);
    databases = map (db: { inherit (db) kind; package = "${dbPackage db}"; }) (def.databases or [ ]);
    hardening = def.hardening or { };
    resources = def.resources or { };
    backup = def.backup or { };
  } // lib.optionalAttrs (def ? pre_start) {
    pre_start = "${pkg}/${def.pre_start}";
  } // lib.optionalAttrs (def ? pre_start_timeout) {
    inherit (def) pre_start_timeout;
  };
in
assert lib.assertMsg (!builtins.pathExists (dir + "/override.nix"))
  "coolix: apps/${name}/override.nix is no longer supported: units are rendered by root. Use path, pre_start, [resources] and [hardening] in app.toml instead.";
{
  inherit manifest;
}
