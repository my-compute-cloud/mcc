{
  lib,
  buildGoModule,
  version,
  # The commit the source is at, if known. It is part of what the agent calls itself and not
  # of the package's name, so that the Go modules are not fetched again for every commit.
  rev ? null,
}:

buildGoModule {
  pname = "mcc-agent";
  inherit version;
  src = lib.cleanSource ../agent;
  vendorHash = "sha256-UCn60zreYDR5yiaFUq/2hLIwJZyKLaDbDYDXIOKGglY=";
  subPackages = [ "cmd/mcc" ];
  ldflags = [
    "-s"
    "-w"
    "-X github.com/my-compute-cloud/mcc/agent/internal/agent.Version=${version}${
      lib.optionalString (rev != null) "+${rev}"
    }"
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
