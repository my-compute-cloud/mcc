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
  # One file per cloud, named as the agent looks for them (MCC_CLOUD_POLICY_DIR).
  cloudPolicies = pkgs.linkFarm "mcc-cloud-policies" (
    lib.mapAttrsToList (name: policy: {
      name = "${name}.json";
      path = pkgs.writeText "mcc-cloud-${name}.json" (builtins.toJSON policy);
    }) cfg.clouds
  );

  # Where launchd finds the agent's launcher: in the system profile, under a name that does
  # not change from one version to the next.
  #
  #   - The service's definition is then the same for every version, so nix-darwin, which
  #     reloads a service only when its definition differs, leaves a running agent alone. An
  #     update installs the new agent; the running one finishes its work and then replaces
  #     itself with what this path leads to (MCC_UPGRADE_PATH, docs/agent.md).
  #   - It is the profile and not /run/current-system because of when each is switched. The
  #     profile is set before activation runs; /run/current-system only after services have
  #     been loaded, when it still leads to the old system or, on a first install, nowhere.
  #   - macOS shows the file's name to the user, in the "can run in the background" notice
  #     and in Login Items: mcc-agent, where a store path would be named by its hash.
  launcher = "/nix/var/nix/profiles/system/sw/bin/mcc-agent";

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
    ${lib.optionalString (cfg.clouds != { }) "export MCC_CLOUD_POLICY_DIR=${cloudPolicies}"}
    if [ ! -f "$MCC_HOME/state.json" ] && ! ls "$MCC_HOME"/clouds/*/state.json >/dev/null 2>&1; then
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
        allowed_images = [ "ghcr.io/my-compute-cloud/*" ];
        excluded_images = [ "ghcr.io/my-compute-cloud/macos-runner:sequoia" ];
        allowed_github_urls = [ "https://github.com/your-org" ];
        active_hours = "22:00-07:00";
      };
      description = ''
        Local policy, as the fields of policy.json (see docs/agent.md). When empty, the agent
        reads policy.json from the state directory, or uses its defaults.
      '';
    };

    clouds = lib.mkOption {
      type = lib.types.attrsOf (lib.types.attrsOf lib.types.anything);
      default = { };
      example = {
        acme = {
          max_vms = 1;
          allowed_github_urls = [ "https://github.com/acme" ];
        };
      };
      description = ''
        What each other cloud this machine is enrolled in may do here, by the name given to
        `mcc enroll --cloud NAME`: the fields of a cloud's policy.json (see docs/agent.md). A
        cloud not named here runs GitHub Actions jobs in VMs and nothing else. When empty, the
        agent reads clouds/NAME/policy.json from the state directory.
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

    # An update that changes the agent restarts nothing, so it says what will happen.
    system.activationScripts.postActivation.text = ''
      echo "mcc: the agent is not restarted by this. If a newer one was installed, the running one switches to it once it is idle; \`mcc check\` says where that stands." >&2
    '';

    launchd.user.agents.mcc-agent.serviceConfig = {
      ProgramArguments = [ launcher ];
      EnvironmentVariables.MCC_UPGRADE_PATH = launcher;
      RunAtLoad = true;
      KeepAlive.SuccessfulExit = false;
      ThrottleInterval = 30;
      ProcessType = "Interactive";
      StandardOutPath = "${home}/Library/Logs/mcc-agent.log";
      StandardErrorPath = "${home}/Library/Logs/mcc-agent.log";
    };
  };
}
