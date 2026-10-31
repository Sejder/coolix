{ pkgs, lib, inputs, system, name, dir }:

let
  def = builtins.fromTOML (builtins.readFile (dir + "/app.toml"));

  input = inputs."app-${name}" or (throw
    "coolix: flake input app-${name} is missing; run `coolix deploy` to regenerate flake.nix");
  pkg = input.packages.${system}.${def.package or "default"};
  exe = if def ? command then "${pkg}/${def.command}" else lib.getExe pkg;

  ports = {
    blue = def.port_base;
    green = def.port_base + 1;
  };

  # systemd EnvironmentFile syntax: KEY="value" with \ and " escaped.
  envFile = slot: lib.concatStrings (lib.mapAttrsToList
    (k: v: "${k}=\"${lib.escape [ "\\" "\"" ] (toString v)}\"\n")
    ((def.env or { }) // {
      PORT = ports.${slot};
      COOLIX_APP = name;
      COOLIX_SLOT = slot;
    }));

  envDir = pkgs.runCommandLocal "coolix-${name}-env"
    {
      blue = envFile "blue";
      green = envFile "green";
      passAsFile = [ "blue" "green" ];
    } ''
    mkdir $out
    cp $bluePath $out/blue.env
    cp $greenPath $out/green.env
  '';

  # Hardened by default; app.toml [hardening] overrides individual keys.
  hardening = {
    DynamicUser = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    PrivateTmp = true;
    PrivateDevices = true;
    NoNewPrivileges = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    ProtectClock = true;
    ProtectHostname = true;
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    MemoryDenyWriteExecute = true;
    RestrictAddressFamilies = "AF_INET AF_INET6 AF_UNIX";
    SystemCallArchitectures = "native";
    SystemCallFilter = "@system-service";
    CapabilityBoundingSet = "";
    UMask = "0077";
  } // (def.hardening or { });

  override =
    if builtins.pathExists (dir + "/override.nix")
    then import (dir + "/override.nix") { inherit pkgs lib pkg name; app = def; }
    else { };

  unitName = "coolix-app-${name}@.service";

  unitAttrs = lib.recursiveUpdate
    {
      Unit = {
        Description = "coolix app ${name} (%i)";
        After = "network.target";
      };
      Service = hardening // {
        Type = "simple";
        ExecStart = exe;
        EnvironmentFile = "${envDir}/%i.env";
        Restart = "on-failure";
        RestartSec = 2;
        StateDirectory = "coolix-apps/${name}";
        WorkingDirectory = "%S/coolix-apps/${name}";
        SyslogIdentifier = "coolix-${name}";
      };
    }
    override;

  toUnit = lib.generators.toINI {
    listsAsDuplicateKeys = true;
    mkKeyValue = lib.generators.mkKeyValueDefault
      {
        mkValueString = v:
          if v == true then "yes"
          else if v == false then "no"
          else toString v;
      } "=";
  };
in
{
  inherit unitName;
  unit = pkgs.writeText "coolix-app-${name}-unit" (toUnit unitAttrs);
  manifest = {
    unit = unitName;
    inherit ports;
    domains = def.domains or [ ];
    health = def.health or { };
  };
}
