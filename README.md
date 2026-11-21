# hush

A Signal soft-block. For each conversation you pause, hush:

- **saves** every message (and attachment) to an archive on the server;
- **hides** it from all your devices ("delete for me" sync), without the
  sender knowing;
- **answers** with an auto-reply giving your reason and the end time. One
  reply stays up while they keep writing, and it's **deleted** for everyone
  once they've been quiet for `replyTtl` (default 5m). If they write again
  after that, they get a fresh one, so a deleted reply is never left
  unexplained. A changed block (new reason or end time) sends the new reply
  before deleting the old one;
- has an **escape hatch**: a message starting with the escape word (default
  `URGENT`) opens a window where their messages come through normally. They
  get a notice saying so. The window stays open while either of you keeps
  writing and closes after `escapeWindow` (default 2m) of quiet. Then the
  notice deletes itself and the pause resumes. Messages in the window are
  still archived, marked as escaped.

You drive it from **Note to Self**. Nothing is pushed to you: held messages
are only shown when you ask.

hush runs beside a patched `signal-cli` linked as a secondary device on your
account. Upstream signal-cli can't send "delete for me", so
`nix/packages/signal-cli-hush` adds a `deleteForMe` command
(`SyncMessage.DeleteForMe`, the same sync your phone sends).

## Commands (Note to Self)

```
/hush block <who> [3d | 12h | until 2026-12-01 [18:00]] [reason…]
/hush unblock <who>
/hush list
/hush read <who> [count]
/hush help
```

`<who>` is a contact or group name (case-insensitive; a unique prefix works;
quote names with spaces), a `+number`, or a uuid. Names you set beat names
people set themselves: your Signal nicknames first, then your contact
(address-book) names and group names, then profile names. So if a stranger's
profile name is the same as your contact name for someone, the contact name
wins. A real collision lists the matches with the last digits of each number. Without a duration, a block
lasts until you unblock. Blocking again updates the block. Blocks end
silently when they expire.

Your commands and hush's `[hush]` answers clear themselves from Note to Self
after `commandTtl` (default 10m) unless `keepCommands` is set.

## What gets through, and what doesn't

Only messages that show up as a bubble are acted on: text, attachments,
stickers, shared contacts, polls, and edits of those. Anything else in a
blocked conversation is ignored: no archive, no delete, no reply. That covers
profile and avatar changes, profile-key updates, disappearing-timer changes,
typing, receipts, reactions, remote deletes, poll votes, pins, and group
metadata.

Limits that come from Signal itself:

- **Your phone still notifies when the message arrives**, before the
  delete-for-me sync lands a moment later. **Mute blocked conversations** on
  your phone. hush can't set mute yet: signal-cli has no command for it, and
  writing Signal's storage service from a linked device is risky.
- **Reactions** to your messages still appear (they aren't messages, so they
  can't be deleted for you), and **calls** still ring.
- The other side sees the auto-reply turn into "This message was deleted".
- Remote delete only works within 24h, so `replyTtl` must be under 23h.

## What is stored

Only blocked messages are persisted:

- `archive/<conversation>/messages.jsonl`: one line per message, with
  signal-cli's raw envelope; `attachments/` beside it.
- `state.json`: the block list, auto-replies and command messages still to
  be deleted. No message content.

signal-cli downloads every attachment it receives. hush deletes each download
right after handling it, and sweeps anything left over after 2 minutes. Logs
(hush's and signal-cli's, which runs with `--scrub-log`) carry no names,
numbers, uuids, group ids, or message content. signal-cli does keep what it
needs to work: keys, sessions, contacts, groups, and a short-lived log of its
own sent messages for retries.

If a message can't be archived (say an attachment fetch fails), hush leaves it
in place rather than delete something it hasn't saved.

## NixOS

```nix
imports = [ "${hush}/nix/module.nix" ];
services.hush = {
  enable = true;
  settings = {
    escape = "URGENT";              # "" disables the escape hatch
    escapeWindow = "2m";            # escape window closes after this much quiet
    timezone = "America/Los_Angeles";
    replyTtl = "5m";                # reply stays up this long after their last message
    commandTtl = "10m";
    # template = "…";               # Go text/template: .Name .Reason .Until .Escape .ReplyTTL
    # escapeTemplate = "…";         # .Name .Window
  };
};
```

Then link once: `sudo hush-link`, and scan the QR code from your phone
(Settings → Linked devices → Link new device). Send `/hush help` to Note to
Self.

`hush -check -config …` validates a config and prints a sample reply. Changes
to `settings` reload hush via SIGHUP, with no restart.

## Development

`nix-shell -A shell` (or direnv), then `go test ./...`. The signal-cli patch
is built against nixpkgs' signal-cli (`nix-build -A packages.signal-cli-hush`);
it applies cleanly to 0.14.6 and 0.14.9.
