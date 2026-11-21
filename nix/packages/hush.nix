{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "hush";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../go.mod
      ../../cmd
      ../../internal
    ];
  };

  # Standard library only.
  vendorHash = null;

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "Signal soft-block daemon: archive, hide, and auto-answer messages from paused conversations";
    license = lib.licenses.gpl3Only;
    mainProgram = "hush";
    platforms = lib.platforms.linux;
  };
}
