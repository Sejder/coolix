{ self }:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.coolix;

  # The CLI is wrapped with everything it needs to know about this host:
  # which nixpkgs/coolix lib the apps repo pins, and the tools it shells out to.
  coolix = pkgs.runCommand "coolix-wrapped" { nativeBuildInputs = [ pkgs.makeWrapper ]; } ''
    makeWrapper ${lib.getExe cfg.package} $out/bin/coolix \
      --set COOLIX_STATE_DIR ${cfg.stateDir} \
      --set COOLIX_USER coolix \
      --set COOLIX_NIXPKGS ${pkgs.path} \
      --set COOLIX_LIB ${self} \
      --set COOLIX_SYSTEM ${pkgs.stdenv.hostPlatform.system} \
      --set COOLIX_PORT_MIN ${toString cfg.portRange.from} \
      --set COOLIX_PORT_MAX ${toString cfg.portRange.to} \
      --prefix PATH : ${lib.makeBinPath [ pkgs.git pkgs.util-linux config.nix.package config.systemd.package ]}
  '';
in
{
  options.services.coolix = {
    enable = lib.mkEnableOption "coolix, a NixOS-native self-hosting PaaS";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.coolix;
      defaultText = lib.literalExpression "coolix.packages.\${system}.coolix";
      description = "The coolix package.";
    };

    stateDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/coolix";
      description = "Where coolix keeps the apps repo and operational state.";
    };

    portRange = {
      from = lib.mkOption {
        type = lib.types.port;
        default = 20000;
        description = "First port coolix may allocate to apps (each app takes two: blue and green).";
      };
      to = lib.mkOption {
        type = lib.types.port;
        default = 29999;
        description = "Last port coolix may allocate to apps.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [{
      assertion = cfg.portRange.from < cfg.portRange.to;
      message = "services.coolix.portRange.from must be lower than portRange.to";
    }];

    nix.settings.experimental-features = [ "nix-command" "flakes" ];

    environment.systemPackages = [ coolix ];

    # Owns the apps repo and runs builds. It can start coolix-activate@ units
    # (and only those), which makes it able to deploy arbitrary units: treat
    # access to this user as root-equivalent. The split keeps root out of
    # git/nix evaluation of app code.
    users.users.coolix = {
      isSystemUser = true;
      group = "coolix";
      home = cfg.stateDir;
      extraGroups = [ "systemd-journal" ];
    };
    users.groups.coolix = { };

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0750 coolix coolix -"
      "d /nix/var/nix/profiles/coolix 0755 root root -"
    ];

    # Privileged half of a deploy. The instance is the profile's store path
    # basename; `coolix activate-store` validates it before touching anything.
    systemd.services."coolix-activate@" = {
      description = "Activate coolix app profile %i";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${coolix}/bin/coolix activate-store %i";
      };
    };

    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (action.id == "org.freedesktop.systemd1.manage-units" &&
            subject.user == "coolix" &&
            action.lookup("verb") == "start" &&
            /^coolix-activate@[0-9a-z]{32}-coolix-app-profile\.service$/.test(action.lookup("unit"))) {
          return polkit.Result.YES;
        }
      });
    '';

    # App units live in /run/systemd/system (outside NixOS' control), so they
    # need to be re-linked from the current app profile on every boot.
    systemd.services.coolix-activate-boot = {
      description = "Activate the current coolix app profile";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" "local-fs.target" ];
      unitConfig.ConditionPathExists = "/nix/var/nix/profiles/coolix/apps";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${coolix}/bin/coolix activate";
      };
    };
  };
}
