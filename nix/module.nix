# NixOS module for hush.
#
# Runs two units as the `hush` user:
#   hush-signal-cli — patched signal-cli, linked as a secondary device of your
#                     account, serving JSON-RPC on a unix socket
#   hush            — the soft-block daemon
#
# One-time setup, after the first deploy: run `sudo hush-link` and scan the
# QR code from your phone (Settings → Linked devices → Link new device). The
# units start once linked. Then drive it from Note to Self: `/hush help`.
#
# Persistence: only blocked messages (and their attachments) are kept, under
# /var/lib/hush/archive, plus hush's state (the block list, deletions still
# owed, reply cooldowns). signal-cli's attachment downloads are emptied as they
# arrive; neither unit logs names, numbers or content.
#
# Edits to `settings` reload hush without a restart.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.hush;
  json = pkgs.formats.json { };
  stateDir = "/var/lib/hush";
  signalDir = "${stateDir}/signal-cli";
  socket = "/run/hush-signal-cli/socket";

  configFile = json.generate "hush.json" (
    {
      signalCli = "unix:${socket}";
      archiveDir = "${stateDir}/archive";
      stateFile = "${stateDir}/state.json";
      # hush empties this, so only blocked messages' attachments persist.
      signalCliAttachments = "${signalDir}/attachments";
    }
    // cfg.settings
  );

  signalCli = lib.getExe cfg.signalCliPackage;

  hush-link = pkgs.writeShellApplication {
    name = "hush-link";
    runtimeInputs = [
      pkgs.qrencode
      pkgs.util-linux
    ];
    text = ''
      # Link hush as a new device on your Signal account.
      name=''${1:-hush}
      if [ "$(id -u)" -ne 0 ]; then
        echo "run as root (it switches to the hush user)" >&2
        exit 1
      fi
      systemctl stop hush.service hush-signal-cli.service
      echo "On your phone: Settings → Linked devices → Link new device, then scan:"
      runuser -u hush -- ${signalCli} --scrub-log --config ${signalDir} link -n "$name" |
        while read -r line; do
          case $line in
            sgnl://*) qrencode -t ansiutf8 "$line" ;;
            *) echo "$line" ;;
          esac
        done
      systemctl start hush-signal-cli.service hush.service
      echo "Linked. Send yourself '/hush help' in Note to Self."
    '';
  };

  hardening = {
    NoNewPrivileges = true;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectControlGroups = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    RestrictAddressFamilies = [
      "AF_UNIX"
      "AF_INET"
      "AF_INET6"
    ];
    ReadWritePaths = [ stateDir ];
    UMask = "0077";
  };

  linked = "${signalDir}/data/accounts.json";
in
{
  options.services.hush = {
    enable = lib.mkEnableOption "hush, the Signal soft-block daemon";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./packages/hush.nix { };
      description = "hush package.";
    };

    signalCliPackage = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./packages/signal-cli-hush/package.nix { };
      description = "signal-cli with hush's deleteForMe patch.";
    };

    settings = lib.mkOption {
      type = json.type;
      default = { };
      example = lib.literalExpression ''
        {
          escape = "URGENT";
          escapeWindow = "2m";
          timezone = "America/Los_Angeles";
          replyTtl = "5m";
          commandTtl = "10m";
        }
      '';
      description = "hush config: reply policy. Blocks are managed from Note to Self. See the README.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.hush = {
      isSystemUser = true;
      group = "hush";
      home = stateDir;
    };
    users.groups.hush = { };

    systemd.tmpfiles.rules = [
      "d ${stateDir} 0700 hush hush -"
      "d ${signalDir} 0700 hush hush -"
    ];

    environment.systemPackages = [ hush-link ];
    environment.etc."hush/config.json".source = configFile;

    systemd.services.hush-signal-cli = {
      description = "signal-cli for hush";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      # Not linked yet: run hush-link.
      unitConfig.ConditionPathExists = linked;
      serviceConfig = hardening // {
        User = "hush";
        Group = "hush";
        RuntimeDirectory = "hush-signal-cli";
        # --scrub-log: no numbers, uuids or group ids in the journal.
        # --ignore-*: download nothing hush doesn't need.
        # Never --send-read-receipts: the other side must not see "read".
        ExecStart = lib.concatStringsSep " " [
          signalCli
          "--scrub-log"
          "--config ${signalDir}"
          "daemon"
          "--socket ${socket}"
          "--ignore-stories"
          "--ignore-avatars"
          "--ignore-stickers"
        ];
        Restart = "always";
        RestartSec = 10;
      };
    };

    systemd.services.hush = {
      description = "hush Signal soft-block daemon";
      wantedBy = [ "multi-user.target" ];
      after = [ "hush-signal-cli.service" ];
      bindsTo = [ "hush-signal-cli.service" ];
      unitConfig.ConditionPathExists = linked;
      reloadTriggers = [ configFile ];
      serviceConfig = hardening // {
        User = "hush";
        Group = "hush";
        ExecStartPre = "${lib.getExe cfg.package} -config /etc/hush/config.json -check";
        ExecStart = "${lib.getExe cfg.package} -config /etc/hush/config.json";
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "always";
        RestartSec = 5;
      };
    };
  };
}
