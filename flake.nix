{
  description = "mcc: the My Compute Cloud agent";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Written by the release workflow: the version and, per system, the signed binary's URL
      # and SHA-256. Absent until the first release.
      release =
        if builtins.pathExists ./release.json then
          builtins.fromJSON (builtins.readFile ./release.json)
        else
          null;
      sourceVersion = if release != null then release.version else "0.0.0-dev";
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          system = pkgs.stdenv.hostPlatform.system;
          asset = if release != null then release.assets.${system} or null else null;

          # Built from the source in this repository.
          agent-source = pkgs.callPackage ./nix/agent.nix { version = sourceVersion; };

          # The signed, notarized binary from the GitHub release, where one exists for this system.
          agent-signed = pkgs.stdenvNoCC.mkDerivation {
            pname = "mcc-agent";
            version = release.version;
            src = pkgs.fetchurl { inherit (asset) url sha256; };
            sourceRoot = ".";
            dontBuild = true;
            # Any change to the file would break Apple's signature.
            dontFixup = true;
            installPhase = ''
              install -Dm755 mcc $out/bin/mcc
            '';
            meta = {
              description = "My Compute Cloud agent (signed release binary)";
              homepage = "https://github.com/my-compute-cloud/mcc";
              mainProgram = "mcc";
              sourceProvenance = [ pkgs.lib.sourceTypes.binaryNativeCode ];
            };
          };
        in
        rec {
          inherit agent-source;
          agent = if asset != null then agent-signed else agent-source;
          default = agent;
        }
      );

      # nix-darwin module: runs the agent as a launchd agent in the owner's login session.
      darwinModules.default = import ./nix/darwin-module.nix { inherit self; };
      darwinModules.mcc-agent = self.darwinModules.default;

      checks = forAllSystems (pkgs: {
        agent = self.packages.${pkgs.stdenv.hostPlatform.system}.agent-source;
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-rfc-style);
    };
}
