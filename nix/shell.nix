{
  mkShell,
  go,
  gopls,
  signal-cli-hush,
  qrencode,
}:
mkShell {
  packages = [
    go
    gopls
    signal-cli-hush
    qrencode
  ];
}
