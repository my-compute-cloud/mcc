# nix-darwin module for the MCC agent:
#
#   imports = [ mcc.darwinModules.default ];
#   services.mcc-agent.enable = true;
#
# Enrollment is a one-time manual step, because the token is a secret that should not be in
# the Nix store: create a token in the control plane, then run
#   mcc enroll --server https://<control plane> <token>
{ self }:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.mcc-agent;
  home = "/Users/${cfg.user}";
  policyFile = pkgs.writeText "mcc-policy.json" (builtins.toJSON cfg.policy);

  # The service's program. macOS shows the name of this file to the user, in the "can run in
  # the background" notice and in Login Items, so it is a file called mcc-agent, and launchd is
  # pointed at it through the system profile: a path that stays the same from one version to
  # the next, where a store path would put a hash in front of the user and change every time.
  start = pkgs.writeShellScriptBin "mcc-agent" ''
    set -euo pipefail
    export PATH=${
      lib.makeBinPath [
        cfg.package
        pkgs.tart
      ]
    }:/usr/bin:/bin:/usr/sbin:/sbin
    export MCC_HOME=${lib.escapeShellArg cfg.stateDir}
    ${lib.optionalString (cfg.policy != { }) "export MCC_POLICY_FILE=${policyFile}"}
    if [ ! -f "$MCC_HOME/state.json" ]; then
      echo "The MCC agent is not enrolled; run: mcc enroll --server URL TOKEN" >&2
      # A clean exit, so that launchd does not keep restarting an unenrolled agent.
      exit 0
    fi
    exec mcc run
  '';
in
{
  options.services.mcc-agent = {
    enable = lib.mkEnableOption "the My Compute Cloud agent, which runs jobs in ephemeral Tart VMs";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.agent;
      defaultText = lib.literalExpression "mcc.packages.<system>.agent";
      description = "The agent package.";
    };

    user = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = config.system.primaryUser;
      defaultText = lib.literalExpression "config.system.primaryUser";
      description = "The user whose login session runs the agent and its VMs.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "${home}/Library/Application Support/mcc";
      defaultText = lib.literalExpression ''"/Users/<user>/Library/Application Support/mcc"'';
      description = "Where the agent keeps its key, enrollment state and runner cache.";
    };

    policy = lib.mkOption {
      type = lib.types.attrsOf lib.types.anything;
      default = { };
      example = {
        max_vms = 2;
        allowed_images = [ "ghcr.io/cirruslabs/*" ];
        allowed_github_urls = [ "https://github.com/your-org" ];
        active_hours = "22:00-07:00";
      };
      description = ''
        Local policy, as the fields of policy.json (see docs/agent.md). When empty, the agent
        reads policy.json from the state directory, or uses its defaults.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = pkgs.stdenv.hostPlatform.isAarch64;
        message = "services.mcc-agent needs Apple Silicon: Tart does not run on Intel Macs.";
      }
      {
        assertion = cfg.user != null;
        message = "services.mcc-agent.user or system.primaryUser must be set.";
      }
    ];

    environment.systemPackages = [
      cfg.package
      start
    ];

    launchd.user.agents.mcc-agent.serviceConfig = {
      ProgramArguments = [ "/run/current-system/sw/bin/mcc-agent" ];
      # The path above no longer changes with the version, so this does: nix-darwin restarts
      # an agent whose definition changed, and a new agent should replace the old one.
      EnvironmentVariables.MCC_AGENT_BUILD = "${start}";
      RunAtLoad = true;
      KeepAlive.SuccessfulExit = false;
      ThrottleInterval = 30;
      ProcessType = "Interactive";
      StandardOutPath = "${home}/Library/Logs/mcc-agent.log";
      StandardErrorPath = "${home}/Library/Logs/mcc-agent.log";
    };
  };
}
