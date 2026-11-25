{ lib, buildGoModule }:
buildGoModule {
  pname = "hs-hub";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../main.go
      ../internal
    ];
  };

  # Standard library only.
  vendorHash = null;

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Publishes services announced by tailnet nodes as Headscale extra DNS records";
    mainProgram = "hs-hub";
    platforms = lib.platforms.linux;
  };
}
