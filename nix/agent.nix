{
  lib,
  buildGoModule,
  version,
}:

buildGoModule {
  pname = "mcc-agent";
  inherit version;
  src = lib.cleanSource ../agent;
  vendorHash = "sha256-7JqwcenKhaOgziXfkl32qz2VrZ0zKhNJMLfvHto/Pco=";
  subPackages = [ "cmd/mcc" ];
  ldflags = [
    "-s"
    "-w"
    "-X github.com/my-compute-cloud/mcc/agent/internal/agent.Version=${version}"
  ];
  # The agent tests talk to an in-process WebSocket server on the loopback interface.
  __darwinAllowLocalNetworking = true;
  # Tests run for every package in the module, not just the one that is built.
  preCheck = ''
    unset subPackages
  '';

  meta = {
    description = "My Compute Cloud agent: runs work from the control plane in short-lived VMs";
    homepage = "https://github.com/my-compute-cloud/mcc";
    mainProgram = "mcc";
  };
}
