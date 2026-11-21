// Package softblock is hush's core: for each message in a blocked
// conversation, archive it, delete it from this account's devices, and
// (rate-limited) reply with a notice that later deletes itself. Blocks are
// managed with /hush commands in Note to Self (see commands.go).
package softblock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/psyclyx/hush/internal/config"
	"github.com/psyclyx/hush/internal/signal"
)

// Signal is the slice of signal-cli that hush drives.
type Signal interface {
	Send(ctx context.Context, conv signal.Conversation, text string) (int64, error)
	RemoteDelete(ctx context.Context, conv signal.Conversation, ts int64) error
	DeleteForMe(ctx context.Context, conv signal.Conversation, author string, ts ...int64) error
	Attachment(ctx context.Context, conv signal.Conversation, id string) ([]byte, error)
	Contacts(ctx context.Context) ([]signal.Contact, error)
	Groups(ctx context.Context) ([]signal.Group, error)
	// Self is this account's number.
	Self(ctx context.Context) (string, error)
}

// Block soft-blocks one conversation.
type Block struct {
	Name string `json:"name"`
	// Conversation is a 1:1 recipient (uuid, else number) or a group. It
	// also keys the archive, so it never changes once set.
	Conversation signal.Conversation `json:"conversation"`
	// Number also matches a 1:1 sender, when known.
	Number string     `json:"number,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Since  time.Time  `json:"since"`
	Until  *time.Time `json:"until,omitempty"`
}

func (b *Block) Active(now time.Time) bool { return b.Until == nil || now.Before(*b.Until) }

type Daemon struct {
	cfg     *config.Config
	sig     Signal
	archive *Archive
	state   *State
	now     func() time.Time
}

func New(cfg *config.Config, sig Signal) (*Daemon, error) {
	st, err := LoadState(cfg.StateFile)
	if err != nil {
		return nil, err
	}
	return &Daemon{cfg: cfg, sig: sig, archive: &Archive{Dir: cfg.ArchiveDir}, state: st, now: time.Now}, nil
}

// Reload swaps in a new config; state carries over.
func (d *Daemon) Reload(cfg *config.Config) {
	d.cfg = cfg
	d.archive = &Archive{Dir: cfg.ArchiveDir}
}

// Run handles envelopes and sweeps due deletions until ctx is done.
// Everything happens on this goroutine, so state needs no locking.
func (d *Daemon) Run(ctx context.Context, received <-chan signal.Received, reload <-chan *config.Config) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	d.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case cfg := <-reload:
			d.Reload(cfg)
			slog.Info("config reloaded")
		case r := <-received:
			d.Handle(ctx, r)
		case <-tick.C:
			d.Sweep(ctx)
		}
	}
}

// match finds the active block covering a message, and the conversation the
// message arrived in (which is where replies and deletes are addressed).
func (d *Daemon) match(env *signal.Envelope, dm *signal.DataMessage) (*Block, signal.Conversation) {
	now := d.now()
	if dm.GroupInfo != nil {
		conv := signal.Conversation{GroupID: dm.GroupInfo.GroupID}
		for i := range d.state.Blocks {
			b := &d.state.Blocks[i]
			if b.Conversation.GroupID == conv.GroupID && b.Active(now) {
				return b, conv
			}
		}
		return nil, conv
	}
	conv := signal.Conversation{Recipient: env.Source()}
	for i := range d.state.Blocks {
		b := &d.state.Blocks[i]
		r := b.Conversation.Recipient
		if r == "" || !b.Active(now) {
			continue
		}
		if r == env.SourceUUID || r == env.SourceNumber || (b.Number != "" && b.Number == env.SourceNumber) {
			return b, conv
		}
	}
	return nil, conv
}

// Handle processes one envelope. Logging here never names a sender, a
// conversation or content: only that something happened.
func (d *Daemon) Handle(ctx context.Context, r signal.Received) {
	env := &r.Envelope
	// signal-cli downloads every attachment it sees; only blocked ones are
	// meant to persist, and those were copied into the archive by now.
	defer d.dropAttachments(env)
	if sm, ok := env.NoteToSelf(); ok {
		d.noteToSelf(ctx, sm)
		return
	}
	if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
		d.youWrote(env.SyncMessage.SentMessage)
		return
	}
	// A new message, or an edit of one (which is addressed by the original's
	// sent timestamp). Receipts, typing, reactions and sync messages pass.
	dm, target := env.DataMessage, env.Timestamp
	if env.EditMessage != nil {
		dm, target = env.EditMessage.DataMessage, env.EditMessage.TargetSentTimestamp
	}
	if dm == nil || !dm.HasContent() {
		return
	}
	b, conv := d.match(env, dm)
	if b == nil {
		return
	}
	// The escape word opens a window; anything sent while it's open passes.
	escape := d.cfg.EscapeWord()
	open := d.live(conv, noticeEscape) >= 0
	escaped := open || (escape != "" && hasPrefixFold(strings.TrimSpace(dm.Message), escape))

	// Never delete what we failed to save.
	if err := d.archive.Save(ctx, d.sig, b.Conversation, conv, Record{
		ReceivedAt: d.now(),
		Block:      b.Name,
		Escaped:    escaped,
		Envelope:   r.Raw,
	}, dm.Attachments); err != nil {
		slog.Error("archive failed; leaving message in place", "err", scrub(err))
		return
	}
	if escaped {
		if !open {
			slog.Info("escape window opened")
		}
		text, err := RenderEscape(d.cfg, b)
		if err != nil {
			slog.Error("render escape notice", "err", scrub(err))
			return
		}
		d.notice(ctx, conv, noticeEscape, text, d.cfg.EscapeWindow.Duration)
		return
	}
	if err := d.sig.DeleteForMe(ctx, conv, env.Source(), target); err != nil {
		slog.Error("delete-for-me failed", "err", scrub(err))
	} else {
		slog.Info("archived and deleted")
	}
	text, err := Render(d.cfg, b)
	if err != nil {
		slog.Error("render reply", "err", scrub(err))
		return
	}
	d.notice(ctx, conv, noticeReply, text, d.cfg.ReplyTTL.Duration)
}

// youWrote handles a message you sent from another device. Replying inside
// an open escape window keeps it open: you're mid-conversation.
func (d *Daemon) youWrote(sm *signal.SentMessage) {
	if !sm.HasContent() {
		return
	}
	conv := signal.Conversation{Recipient: sm.DestinationUUID}
	if sm.GroupInfo != nil {
		conv = signal.Conversation{GroupID: sm.GroupInfo.GroupID}
	} else if conv.Recipient == "" {
		conv.Recipient = sm.DestinationNumber
	}
	if i := d.live(conv, noticeEscape); i >= 0 {
		d.extend(i, d.cfg.EscapeWindow.Duration)
		d.save()
	}
}

// Notice kinds: the block's auto-reply, and the escape window's notice.
const (
	noticeReply  = "reply"
	noticeEscape = "escape"
)

// live finds the conversation's current notice of a kind; -1 if none is up.
func (d *Daemon) live(conv signal.Conversation, kind string) int {
	now := d.now()
	for i := len(d.state.Pending) - 1; i >= 0; i-- {
		p := &d.state.Pending[i]
		if !p.LocalOnly && p.Conversation == conv && p.noticeKind() == kind && now.Before(p.DeleteAt) {
			return i
		}
	}
	return -1
}

// extend keeps notice i up for ttl from now; false if that would pass
// Signal's 24h remote-delete window.
func (d *Daemon) extend(i int, ttl time.Duration) bool {
	p := &d.state.Pending[i]
	at := d.now().Add(ttl)
	if at.After(p.SentAt.Add(replyMaxAge)) {
		return false
	}
	p.DeleteAt = at
	return true
}

// notice keeps exactly one notice of a kind visible while they keep writing:
// with none up, send one; with one up, push its deletion back to ttl from
// now. So a notice only turns into "This message was deleted" after ttl of
// quiet, and their next message gets a fresh one. If the text changed (new
// reason or end), or the notice is too old to keep (remote delete stops
// working after 24h), the new one goes out before the old one is deleted, so
// an explanation is always on screen.
func (d *Daemon) notice(ctx context.Context, conv signal.Conversation, kind, text string, ttl time.Duration) {
	now := d.now()
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:8])

	i := d.live(conv, kind)
	if i >= 0 && d.state.Pending[i].Digest == digest && d.extend(i, ttl) {
		d.save()
		return
	}
	ts, err := d.sig.Send(ctx, conv, text)
	if err != nil {
		slog.Error("send notice", "kind", kind, "err", scrub(err))
		return
	}
	if i >= 0 {
		d.state.Pending[i].DeleteAt = now // superseded; the new one is up
	}
	d.state.Pending = append(d.state.Pending, PendingDelete{
		Conversation: conv, Timestamp: ts, SentAt: now, DeleteAt: now.Add(ttl), Digest: digest, Kind: kind,
	})
	d.save()
	slog.Info("notice sent", "kind", kind)
}

// replyMaxAge is the latest an auto-reply is deleted, safely inside Signal's
// 24h remote-delete window.
const replyMaxAge = 23 * time.Hour

func (d *Daemon) schedule(conv signal.Conversation, ts int64, at time.Time, localOnly bool) {
	d.state.Pending = append(d.state.Pending, PendingDelete{
		Conversation: conv, Timestamp: ts, SentAt: d.now(), DeleteAt: at, LocalOnly: localOnly,
	})
}

// Render produces a block's auto-reply.
func Render(cfg *config.Config, b *Block) (string, error) {
	data := struct{ Name, Reason, Until, Escape, ReplyTTL string }{
		Name: b.Name, Reason: b.Reason, Escape: cfg.EscapeWord(), ReplyTTL: humanDuration(cfg.ReplyTTL.Duration),
	}
	if b.Until != nil {
		data.Until = b.Until.In(cfg.Location).Format("Monday, Jan 2 at 3:04 PM MST")
	}
	var sb strings.Builder
	if err := cfg.Tmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// RenderEscape produces the notice sent when the escape window opens.
func RenderEscape(cfg *config.Config, b *Block) (string, error) {
	data := struct{ Name, Window string }{Name: b.Name, Window: humanDuration(cfg.EscapeWindow.Duration)}
	var sb strings.Builder
	if err := cfg.EscapeTmpl.Execute(&sb, data); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// Sweep ends expired blocks and deletes messages whose time is up.
// Auto-replies are deleted everywhere: remote delete for the other side, then
// delete-for-me to clear the tombstone on our devices. Note to Self chatter
// only lives on our devices, so delete-for-me alone covers it.
func (d *Daemon) Sweep(ctx context.Context) {
	d.sweepAttachments()
	now := d.now()
	changed := false

	// Expired blocks end silently: no notices about held messages unless
	// asked for with /hush read.
	blocks := d.state.Blocks[:0]
	for _, b := range d.state.Blocks {
		if b.Active(now) {
			blocks = append(blocks, b)
		} else {
			changed = true
			slog.Info("block ended")
		}
	}
	d.state.Blocks = blocks

	kept := d.state.Pending[:0]
	for _, p := range d.state.Pending {
		if now.Before(p.DeleteAt) {
			kept = append(kept, p)
			continue
		}
		var err error
		if !p.LocalOnly {
			err = d.sig.RemoteDelete(ctx, p.Conversation, p.Timestamp)
		}
		if err == nil {
			if err := d.deleteOwn(ctx, p); err != nil {
				slog.Warn("delete-for-me", "err", scrub(err))
			}
			changed = true
			continue
		}
		if now.Sub(p.SentAt) > replyMaxAge {
			slog.Error("giving up deleting auto-reply (past Signal's 24h limit)", "err", scrub(err))
			changed = true
			continue
		}
		slog.Warn("delete auto-reply; will retry", "err", scrub(err))
		kept = append(kept, p)
	}
	d.state.Pending = kept
	if changed {
		d.save()
	}
}

func (d *Daemon) save() {
	if err := d.state.Save(d.cfg.StateFile); err != nil {
		slog.Error("save state", "err", scrub(err))
	}
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return plural(int(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	default:
		return d.String()
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// Record is one archived message, one JSON line per message.
type Record struct {
	ReceivedAt  time.Time       `json:"receivedAt"`
	Block       string          `json:"block"`
	Escaped     bool            `json:"escaped"`
	Envelope    json.RawMessage `json:"envelope"`
	Attachments []string        `json:"attachments,omitempty"`
}

// attachmentIDs lists every attachment an envelope references.
func attachmentIDs(env *signal.Envelope) []string {
	var ids []string
	add := func(dm *signal.DataMessage) {
		if dm == nil {
			return
		}
		for _, a := range dm.Attachments {
			ids = append(ids, a.ID)
		}
	}
	add(env.DataMessage)
	if env.EditMessage != nil {
		add(env.EditMessage.DataMessage)
	}
	if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
		add(&env.SyncMessage.SentMessage.DataMessage)
	}
	return ids
}

// dropAttachments removes signal-cli's downloaded copies of an envelope's
// attachments.
func (d *Daemon) dropAttachments(env *signal.Envelope) {
	dir := d.cfg.SignalCliAttachments
	if dir == "" {
		return
	}
	for _, id := range attachmentIDs(env) {
		if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
			continue
		}
		if err := os.Remove(filepath.Join(dir, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove downloaded attachment", "err", scrub(err))
		}
	}
}

// attachmentMaxAge bounds how long anything hush didn't see referenced (quote
// thumbnails, link previews, files from while hush was down) lingers in
// signal-cli's attachment directory.
const attachmentMaxAge = 2 * time.Minute

func (d *Daemon) sweepAttachments() {
	dir := d.cfg.SignalCliAttachments
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("sweep attachments", "err", scrub(err))
		}
		return
	}
	cutoff := d.now().Add(-attachmentMaxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil {
			slog.Warn("sweep attachments", "err", scrub(err))
		}
	}
}

// deleteOwn clears one of our own messages from our devices.
func (d *Daemon) deleteOwn(ctx context.Context, p PendingDelete) error {
	self, err := d.sig.Self(ctx)
	if err != nil {
		return err
	}
	return d.sig.DeleteForMe(ctx, p.Conversation, self, p.Timestamp)
}
