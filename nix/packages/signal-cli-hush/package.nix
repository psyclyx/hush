# signal-cli plus a `deleteForMe` command: sends a SyncMessage.DeleteForMe to
# this account's other devices, removing messages there without telling the
# other side. Upstream signal-cli has the protobuf but no way to send it.
{ signal-cli }:
signal-cli.overrideAttrs (old: {
  pname = "signal-cli-hush";
  patches = (old.patches or [ ]) ++ [ ./delete-for-me.patch ];
})
