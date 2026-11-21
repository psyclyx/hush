package softblock

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/psyclyx/hush/internal/config"
	"github.com/psyclyx/hush/internal/signal"
)

type call struct {
	Method string
	Conv   signal.Conversation
	Author string
	TS     []int64
	Text   string
}

type fake struct {
	calls     []call
	nextTS    int64
	attachErr error
	contacts  []signal.Contact
	groups    []signal.Group
}

func (f *fake) Send(_ context.Context, c signal.Conversation, text string) (int64, error) {
	f.nextTS++
	f.calls = append(f.calls, call{Method: "send", Conv: c, Text: text, TS: []int64{f.nextTS}})
	return f.nextTS, nil
}
func (f *fake) RemoteDelete(_ context.Context, c signal.Conversation, ts int64) error {
	f.calls = append(f.calls, call{Method: "remoteDelete", Conv: c, TS: []int64{ts}})
	return nil
}
func (f *fake) DeleteForMe(_ context.Context, c signal.Conversation, author string, ts ...int64) error {
	f.calls = append(f.calls, call{Method: "deleteForMe", Conv: c, Author: author, TS: ts})
	return nil
}
func (f *fake) Attachment(_ context.Context, _ signal.Conversation, id string) ([]byte, error) {
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return []byte("bytes of " + id), nil
}
func (f *fake) Contacts(context.Context) ([]signal.Contact, error) { return f.contacts, nil }
func (f *fake) Groups(context.Context) ([]signal.Group, error)     { return f.groups, nil }
func (f *fake) Self(context.Context) (string, error)               { return selfPhone, nil }

func (f *fake) methods() string {
	var m []string
	for _, c := range f.calls {
		m = append(m, c.Method)
	}
	return strings.Join(m, ",")
}

const (
	alice     = "aaaaaaaa-0000-0000-0000-000000000001"
	bob       = "bbbbbbbb-0000-0000-0000-000000000002"
	alicia    = "cccccccc-0000-0000-0000-000000000003"
	selfUUID  = "ffffffff-0000-0000-0000-00000000000f"
	groupID   = "Z3JvdXBpZGdyb3VwaWRncm91cGlkZ3JvdXBpZGdyb3VwaWQ="
	selfPhone = "+15550000000"
)

type harness struct {
	t      *testing.T
	ctx    context.Context
	d      *Daemon
	f      *fake
	now    *time.Time
	attDir string
	selfTS int64
}

func setup(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	attDir := filepath.Join(dir, "signal-attachments")
	if err := os.MkdirAll(attDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	cfg := `{"signalCli":"unix:/x","archiveDir":"` + dir + `/archive","stateFile":"` + dir + `/state.json",
		"signalCliAttachments":"` + attDir + `","timezone":"UTC"}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{nextTS: 1000,
		contacts: []signal.Contact{
			{UUID: alice, Number: "+15551110001", Name: "Alice Smith", GivenName: "Alice"},
			{UUID: bob, Number: "+15551110002", Name: "Bob"},
			{UUID: alicia, Number: "+15551110003", Name: "Alicia Keys"},
		},
		groups: []signal.Group{{ID: groupID, Name: "Book Club", IsMember: true}},
	}
	d, err := New(c, f)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	return &harness{t: t, ctx: context.Background(), d: d, f: f, now: &now, attDir: attDir, selfTS: 5000}
}

func received(e signal.Envelope) signal.Received {
	raw, _ := json.Marshal(e)
	return signal.Received{Envelope: e, Raw: raw}
}

func msg(from string, ts int64, text string) signal.Received {
	return received(signal.Envelope{SourceUUID: from, SourceNumber: "+1555111000" + from[len(from)-1:], Timestamp: ts,
		DataMessage: &signal.DataMessage{Timestamp: ts, Message: text}})
}

func (h *harness) noteToSelf(text string) {
	h.selfTS++
	h.d.Handle(h.ctx, received(signal.Envelope{SourceUUID: selfUUID, SourceNumber: selfPhone, Timestamp: h.selfTS,
		SyncMessage: &signal.SyncMessage{SentMessage: &signal.SentMessage{DestinationUUID: selfUUID,
			DataMessage: signal.DataMessage{Timestamp: h.selfTS, Message: text}}}}))
}

// cmd runs a /hush command and returns the answer, leaving no trace in f.calls.
func (h *harness) cmd(text string) string {
	h.t.Helper()
	n := len(h.f.calls)
	h.noteToSelf(text)
	if len(h.f.calls) != n+1 {
		h.t.Fatalf("%q: want one answer, calls = %s", text, h.f.methods())
	}
	c := h.f.calls[n]
	if c.Method != "send" || !c.Conv.NoteToSelf || !strings.HasPrefix(c.Text, "[hush] ") {
		h.t.Fatalf("%q: answer = %+v", text, c)
	}
	h.f.calls = h.f.calls[:n]
	return c.Text
}

func (h *harness) mustContain(s string, want ...string) {
	h.t.Helper()
	for _, w := range want {
		if !strings.Contains(s, w) {
			h.t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

func TestBlockFlow(t *testing.T) {
	h := setup(t)
	ans := h.cmd("/hush block alice 3d need some space")
	h.mustContain(ans, "Blocked Alice Smith until Mon Nov 23, 12:00 PM", "need some space", "Escape word: URGENT")

	h.d.Handle(h.ctx, msg(alice, 111, "hey"))
	if got := h.f.methods(); got != "deleteForMe,send" {
		t.Fatalf("calls = %s", got)
	}
	del, reply := h.f.calls[0], h.f.calls[1]
	if del.Conv.Recipient != alice || del.Author != alice || del.TS[0] != 111 {
		t.Fatalf("deleteForMe = %+v", del)
	}
	if reply.Conv.Recipient != alice {
		t.Fatalf("reply to %+v", reply.Conv)
	}
	h.mustContain(reply.Text, "until Monday, Nov 23 at 12:00 PM UTC", "need some space", `"URGENT"`, "5 minutes")

	recs := h.archived(signal.Conversation{Recipient: alice})
	if len(recs) != 1 || !strings.Contains(string(recs[0].Envelope), `"hey"`) {
		t.Fatalf("archive = %+v", recs)
	}
}

func (h *harness) archived(key signal.Conversation) []Record {
	h.t.Helper()
	recs, err := h.d.archive.Load(key)
	if err != nil {
		h.t.Fatal(err)
	}
	return recs
}

// bobReplies returns the auto-replies sent to Bob and the ones deleted, in
// order, ignoring Note to Self chatter.
func (h *harness) bobReplies() (sent, deleted []int64) {
	for _, c := range h.f.calls {
		switch {
		case c.Conv.Recipient != bob:
		case c.Method == "send":
			sent = append(sent, c.TS[0])
		case c.Method == "remoteDelete":
			deleted = append(deleted, c.TS[0])
		}
	}
	return sent, deleted
}

func (h *harness) advance(d time.Duration) {
	*h.now = h.now.Add(d)
	h.d.Sweep(h.ctx)
}

func TestReplyStaysUpWhileTheyWrite(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	h.d.Handle(h.ctx, msg(bob, 111, "one"))
	// They keep writing every 4 minutes: one reply, never deleted meanwhile.
	for i := range 5 {
		h.advance(4 * time.Minute)
		h.d.Handle(h.ctx, msg(bob, int64(112+i), "more"))
	}
	sent, deleted := h.bobReplies()
	if len(sent) != 1 || len(deleted) != 0 {
		t.Fatalf("while writing: sent %v, deleted %v", sent, deleted)
	}

	// Quiet: deleted a full ReplyTTL after their last message, not before.
	h.advance(4*time.Minute + 59*time.Second)
	if _, deleted = h.bobReplies(); len(deleted) != 0 {
		t.Fatal("deleted before ReplyTTL of quiet")
	}
	h.advance(2 * time.Second)
	if _, deleted = h.bobReplies(); len(deleted) != 1 || deleted[0] != sent[0] {
		t.Fatalf("not deleted after quiet: %v", deleted)
	}
	// The tombstone on our side goes too.
	if c := h.f.calls[len(h.f.calls)-1]; c.Method != "deleteForMe" || c.Author != selfPhone || c.TS[0] != sent[0] {
		t.Fatalf("tombstone cleanup = %+v", c)
	}

	// Writing again after it's gone: a fresh explanation.
	h.advance(time.Minute)
	h.d.Handle(h.ctx, msg(bob, 200, "hello?"))
	if sent, _ = h.bobReplies(); len(sent) != 2 {
		t.Fatalf("no fresh reply after the old one was deleted: %v", sent)
	}
}

func TestChangedBlockReplacesReplyFirst(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob 3d first reason")
	h.d.Handle(h.ctx, msg(bob, 111, "one"))
	h.advance(time.Minute)
	h.cmd("/hush block Bob 1w new reason")
	h.d.Handle(h.ctx, msg(bob, 112, "two"))
	sent, deleted := h.bobReplies()
	if len(sent) != 2 || len(deleted) != 0 {
		t.Fatalf("sent %v, deleted %v", sent, deleted)
	}
	h.mustContain(h.f.calls[len(h.f.calls)-1].Text, "new reason")
	// The stale one goes on the next sweep, after the new one is up.
	h.advance(time.Second)
	if sent, deleted = h.bobReplies(); len(deleted) != 1 || deleted[0] != sent[0] {
		t.Fatalf("stale reply not replaced: sent %v, deleted %v", sent, deleted)
	}
}

func TestReplyRetiredBeforeSignalLimit(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	// Writing nonstop for a day: the reply can't be kept past Signal's 24h
	// remote-delete window, so it's retired and replaced.
	for i := range 24 * 15 {
		h.d.Handle(h.ctx, msg(bob, int64(1000+i), "x"))
		h.advance(4 * time.Minute)
	}
	sent, deleted := h.bobReplies()
	if len(sent) != 2 || len(deleted) != 1 || deleted[0] != sent[0] {
		t.Fatalf("sent %v, deleted %v", sent, deleted)
	}
	// The replacement went out before the old one was deleted.
	var order []string
	for _, c := range h.f.calls {
		if c.Conv.Recipient == bob && (c.Method == "send" || c.Method == "remoteDelete") {
			order = append(order, c.Method)
		}
	}
	if strings.Join(order, ",") != "send,send,remoteDelete" {
		t.Fatalf("order = %v: deleted before replacing", order)
	}
	for _, p := range h.d.state.Pending {
		if !p.LocalOnly && p.DeleteAt.Sub(p.SentAt) > replyMaxAge {
			t.Fatalf("reply scheduled past the limit: %+v", p)
		}
	}
}

func TestCommandsClearThemselves(t *testing.T) {
	h := setup(t)
	h.noteToSelf("/hush list")
	answerTS := h.f.calls[0].TS[0]
	cmdTS := h.selfTS
	h.f.calls = nil

	*h.now = h.now.Add(9 * time.Minute)
	h.d.Sweep(h.ctx)
	if len(h.f.calls) != 0 {
		t.Fatalf("cleared early: %s", h.f.methods())
	}
	*h.now = h.now.Add(2 * time.Minute)
	h.d.Sweep(h.ctx)
	if got := h.f.methods(); got != "deleteForMe,deleteForMe" {
		t.Fatalf("calls = %s (no remote delete for Note to Self)", got)
	}
	seen := map[int64]bool{}
	for _, c := range h.f.calls {
		if !c.Conv.NoteToSelf || c.Author != selfPhone {
			t.Fatalf("cleanup = %+v", c)
		}
		seen[c.TS[0]] = true
	}
	if !seen[cmdTS] || !seen[answerTS] {
		t.Fatalf("cleared %v; want command %d and answer %d", seen, cmdTS, answerTS)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	h.d.Handle(h.ctx, msg(bob, 111, "hi"))

	d2, err := New(h.d.cfg, h.f)
	if err != nil {
		t.Fatal(err)
	}
	later := h.now.Add(3 * time.Minute)
	d2.now = func() time.Time { return later }
	h.f.calls = nil
	d2.Handle(h.ctx, msg(bob, 112, "still blocked?"))
	if got := h.f.methods(); got != "deleteForMe" {
		t.Fatalf("block or live reply lost on restart: %s", got)
	}
	later = later.Add(5*time.Minute + time.Second)
	d2.Sweep(h.ctx)
	if !strings.Contains(h.f.methods(), "remoteDelete") {
		t.Fatalf("restarted daemon didn't delete reply: %s", h.f.methods())
	}
}

// bobCalls lists the calls touching Bob's conversation, as "method" strings.
func (h *harness) bobCalls() string {
	var m []string
	for _, c := range h.f.calls {
		if c.Conv.Recipient == bob {
			m = append(m, c.Method)
		}
	}
	return strings.Join(m, ",")
}

func TestEscapeWindow(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")

	// The escape word: passes, and opens the window with a notice.
	h.d.Handle(h.ctx, msg(bob, 111, "  urgent: call me, it's about mom"))
	if got := h.bobCalls(); got != "send" {
		t.Fatalf("calls = %s", got)
	}
	notice := h.f.calls[len(h.f.calls)-1]
	h.mustContain(notice.Text, "coming through", "2 minutes")

	// Within the window, plain messages pass too, with no more notices.
	h.f.calls = nil
	h.advance(90 * time.Second)
	h.d.Handle(h.ctx, msg(bob, 112, "are you there"))
	// Your reply from your phone keeps it open as well.
	h.advance(90 * time.Second)
	h.d.Handle(h.ctx, received(signal.Envelope{SourceUUID: selfUUID, Timestamp: 113,
		SyncMessage: &signal.SyncMessage{SentMessage: &signal.SentMessage{DestinationUUID: bob,
			DataMessage: signal.DataMessage{Timestamp: 113, Message: "yes, what's up"}}}}))
	h.advance(90 * time.Second)
	h.d.Handle(h.ctx, msg(bob, 114, "it's about mom"))
	if got := h.bobCalls(); got != "" {
		t.Fatalf("window didn't hold: %s", got)
	}
	recs := h.archived(signal.Conversation{Recipient: bob})
	if len(recs) != 3 || !recs[0].Escaped || !recs[1].Escaped || !recs[2].Escaped {
		t.Fatalf("archive = %+v", recs)
	}

	// Two quiet minutes: the window closes and the notice is deleted.
	h.advance(119 * time.Second)
	if got := h.bobCalls(); got != "" {
		t.Fatalf("closed early: %s", got)
	}
	h.advance(2 * time.Second)
	if got := h.bobCalls(); got != "remoteDelete,deleteForMe" || h.f.calls[0].TS[0] != notice.TS[0] {
		t.Fatalf("notice not deleted at close: %s", got)
	}

	// The pause is back: hidden, with the regular auto-reply.
	h.f.calls = nil
	h.d.Handle(h.ctx, msg(bob, 115, "one more thing"))
	if got := h.bobCalls(); got != "deleteForMe,send" {
		t.Fatalf("after close: %s", got)
	}
	h.mustContain(h.f.calls[1].Text, "paused this conversation")
}

func TestEscapeWindowAlongsideReply(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	h.d.Handle(h.ctx, msg(bob, 111, "hi"))     // hidden, auto-reply
	h.d.Handle(h.ctx, msg(bob, 112, "URGENT")) // window opens
	if got := h.bobCalls(); got != "deleteForMe,send,send" {
		t.Fatalf("calls = %s", got)
	}
	// Each notice keeps its own clock; neither is deleted early or twice.
	h.f.calls = nil
	h.advance(5*time.Minute + time.Second)
	if got := h.bobCalls(); got != "remoteDelete,deleteForMe,remoteDelete,deleteForMe" {
		t.Fatalf("calls = %s", got)
	}
}

func TestEscapeDisabled(t *testing.T) {
	h := setup(t)
	empty := ""
	h.d.cfg.Escape = &empty
	h.cmd("/hush block Bob")
	h.d.Handle(h.ctx, msg(bob, 111, "URGENT please"))
	if got := h.bobCalls(); got != "deleteForMe,send" {
		t.Fatalf("calls = %s", got)
	}
	if strings.Contains(h.f.calls[len(h.f.calls)-1].Text, "start your message") {
		t.Fatal("reply mentions a disabled escape hatch")
	}
}

func TestNotMessagesAreIgnored(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	base := func() signal.Envelope {
		return signal.Envelope{SourceUUID: bob, Timestamp: 111}
	}
	var cases []signal.Envelope
	// Profile key update, expiration timer change, reaction, remote delete,
	// group metadata update: data messages with no content of their own.
	for _, raw := range []string{
		`{"timestamp":111,"isProfileKeyUpdate":true}`,
		`{"timestamp":111,"isExpirationUpdate":true,"expiresInSeconds":3600}`,
		`{"timestamp":111,"reaction":{"emoji":"👍","targetSentTimestamp":1}}`,
		`{"timestamp":111,"remoteDelete":{"timestamp":1}}`,
		`{"timestamp":111,"groupInfo":{"groupId":"x","type":"UPDATE"}}`,
		`{"timestamp":111,"pollVote":{}}`,
	} {
		e := base()
		if err := json.Unmarshal([]byte(raw), &e.DataMessage); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, e)
	}
	// Receipts, typing, calls: no data message at all.
	cases = append(cases, base())
	for _, e := range cases {
		h.d.Handle(h.ctx, received(e))
	}
	if len(h.f.calls) != 0 {
		t.Fatalf("calls = %s", h.f.methods())
	}
	if recs := h.archived(signal.Conversation{Recipient: bob}); len(recs) != 0 {
		t.Fatalf("archived non-messages: %d", len(recs))
	}
}

func TestUnblockedAndOtherNotesIgnored(t *testing.T) {
	h := setup(t)
	h.d.Handle(h.ctx, msg(bob, 111, "not blocked"))
	h.noteToSelf("grocery list: eggs")
	h.noteToSelf("[hush] an answer of ours")
	h.noteToSelf("/hushed is not a command")
	if len(h.f.calls) != 0 {
		t.Fatalf("calls = %s", h.f.methods())
	}
	if _, err := os.Stat(h.d.cfg.ArchiveDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive dir exists without any block: %v", err)
	}
}

func TestBlockExpiresSilently(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob 1h")
	*h.now = h.now.Add(61 * time.Minute)
	h.f.calls = nil
	h.d.Sweep(h.ctx)
	for _, c := range h.f.calls {
		if c.Method == "send" {
			t.Fatalf("sent a notice on expiry: %+v", c)
		}
	}
	h.d.Handle(h.ctx, msg(bob, 111, "after"))
	if strings.Contains(h.f.methods(), "deleteForMe,send") {
		t.Fatalf("expired block still active: %s", h.f.methods())
	}
	h.mustContain(h.cmd("/hush list"), "Nothing is blocked")
}

func TestResolveAndList(t *testing.T) {
	h := setup(t)
	h.mustContain(h.cmd("/hush block ali"), `"ali" is the contact name of 2`, "Alice Smith (…0001)", "Alicia Keys (…0003)", "+number")
	h.mustContain(h.cmd("/hush block nobody"), "no contact or group matches")
	h.mustContain(h.cmd(`/hush block “Alice Smith” until 2026-12-01 18:00 moving`), "Blocked Alice Smith until Tue Dec 1, 6:00 PM", "moving")
	h.mustContain(h.cmd("/hush block +15551110002"), "Blocked Bob indefinitely")
	h.mustContain(h.cmd("/hush block 'book club' 2w"), "Blocked Book Club until Fri Dec 4")
	h.mustContain(h.cmd("/hush block Bob until 2020-01-01"), "in the past")
	h.mustContain(h.cmd("/HUSH LIST"), "• Alice Smith — until Tue Dec 1, 6:00 PM (moving)", "• Bob — indefinitely", "• Book Club")
	h.mustContain(h.cmd("/hush block bob 1d"), "Updated block on Bob")
	h.mustContain(h.cmd("/hush unblock bob"), "Unblocked Bob")
	h.mustContain(h.cmd("/hush unblock bob"), "isn't blocked")
	h.mustContain(h.cmd("/hush frobnicate"), "unknown command", "/hush block")
	h.mustContain(h.cmd("/hush"), "/hush read")
}

func TestReadHeldMessages(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	h.mustContain(h.cmd("/hush read bob"), "Nothing saved from Bob")
	for i, text := range []string{"one", "two", "three"} {
		*h.now = h.now.Add(time.Minute)
		h.d.Handle(h.ctx, msg(bob, int64(200+i), text))
	}
	m := msg(bob, 300, "look")
	m.Envelope.DataMessage.Attachments = []signal.Attachment{{ID: "p.jpg"}}
	h.d.Handle(h.ctx, received(m.Envelope))

	ans := h.cmd("/hush read bob 2")
	h.mustContain(ans, "Bob — last 2 of 4 saved", "three", "look [1 attachment]")
	if strings.Contains(ans, "two") {
		t.Fatalf("count ignored:\n%s", ans)
	}
	// Still readable once unblocked.
	h.cmd("/hush unblock bob")
	h.mustContain(h.cmd("/hush read Bob"), "one", "two", "three")
}

func TestGroupBlock(t *testing.T) {
	h := setup(t)
	h.cmd(`/hush block "Book Club"`)
	m := msg(bob, 111, "group msg")
	m.Envelope.DataMessage.GroupInfo = &signal.GroupInfo{GroupID: groupID}
	h.d.Handle(h.ctx, m)
	if got := h.f.methods(); got != "deleteForMe,send" {
		t.Fatalf("calls = %s", got)
	}
	if c := h.f.calls[0]; c.Conv.GroupID != groupID || c.Author != bob {
		t.Fatalf("deleteForMe = %+v", c)
	}
	if h.f.calls[1].Conv.GroupID != groupID {
		t.Fatalf("reply went to %+v", h.f.calls[1].Conv)
	}
	h.d.Handle(h.ctx, msg(bob, 112, "dm"))
	if len(h.f.calls) != 2 {
		t.Fatalf("group block caught a DM: %s", h.f.methods())
	}
}

func TestEditDeletesOriginal(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	m := received(signal.Envelope{SourceUUID: bob, Timestamp: 222, EditMessage: &signal.EditMessage{
		TargetSentTimestamp: 111, DataMessage: &signal.DataMessage{Timestamp: 222, Message: "edited"}}})
	h.d.Handle(h.ctx, m)
	if c := h.f.calls[0]; c.Method != "deleteForMe" || c.TS[0] != 111 {
		t.Fatalf("calls = %+v", h.f.calls)
	}
}

func TestAttachments(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	touch := func(name string, age time.Duration) string {
		p := filepath.Join(h.attDir, name)
		os.WriteFile(p, []byte("x"), 0o600)
		mt := h.now.Add(-age)
		os.Chtimes(p, mt, mt)
		return p
	}
	blocked := touch("b.jpg", 0)
	unblocked := touch("u.jpg", 0)
	stray := touch("stray.jpg", 5*time.Minute)
	fresh := touch("fresh.jpg", 0)

	m := msg(bob, 111, "")
	m.Envelope.DataMessage.Attachments = []signal.Attachment{{ID: "b.jpg"}}
	h.d.Handle(h.ctx, m)
	m = msg(alice, 112, "")
	m.Envelope.DataMessage.Attachments = []signal.Attachment{{ID: "u.jpg"}}
	h.d.Handle(h.ctx, m)

	for _, p := range []string{blocked, unblocked} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left in signal-cli's dir", filepath.Base(p))
		}
	}
	recs := h.archived(signal.Conversation{Recipient: bob})
	data, err := os.ReadFile(filepath.Join(h.d.archive.convDir(signal.Conversation{Recipient: bob}), recs[0].Attachments[0]))
	if err != nil || string(data) != "bytes of b.jpg" {
		t.Fatalf("blocked attachment not archived: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.d.cfg.ArchiveDir, "dm-"+alice)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unblocked message archived")
	}

	h.d.Sweep(h.ctx)
	if _, err := os.Stat(stray); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale stray attachment survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("sweep removed a fresh attachment hush may still need")
	}
}

func TestArchiveFailureLeavesMessage(t *testing.T) {
	h := setup(t)
	h.cmd("/hush block Bob")
	h.f.attachErr = errors.New("gone")
	m := msg(bob, 111, "see pic")
	m.Envelope.DataMessage.Attachments = []signal.Attachment{{ID: "pic.jpg"}}
	h.d.Handle(h.ctx, m)
	if len(h.f.calls) != 0 {
		t.Fatalf("deleted a message it failed to archive: %s", h.f.methods())
	}
}

func TestParsing(t *testing.T) {
	for in, want := range map[string]time.Duration{"3d": 72 * time.Hour, "1w": 168 * time.Hour, "2d12h": 60 * time.Hour, "90m": 90 * time.Minute} {
		if got, ok := parseDuration(in); !ok || got != want {
			t.Errorf("parseDuration(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"soon", "3", "d", "0m"} {
		if _, ok := parseDuration(in); ok {
			t.Errorf("parseDuration(%q) accepted", in)
		}
	}
	for in, want := range map[string]string{
		` block "Alice  Smith" 3d it's fine`: "block|Alice  Smith|3d|it's|fine",
		`block 'book club' friends' trip`:    "block|book club|friends'|trip",
		"block “Alice Smith” I’m away":       "block|Alice Smith|I'm|away",
		`block 'O'Brien' 1d`:                 "block|O'Brien|1d",
	} {
		toks, err := tokenize(in)
		if got := strings.Join(toks, "|"); err != nil || got != want {
			t.Errorf("tokenize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := tokenize(`block "unclosed`); err == nil {
		t.Error("unclosed quote accepted")
	}
}

func TestScrub(t *testing.T) {
	got := scrub(errors.New("Unregistered user: +15551234567 (" + alice + ") in " + groupID))
	if strings.ContainsAny(got, "0123456789") && strings.Contains(got, "5551234567") || strings.Contains(got, alice) || strings.Contains(got, groupID) {
		t.Fatalf("scrub left identifiers: %s", got)
	}
}

func TestConfigValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(path, []byte(`{"signalCli":"unix:/x","archiveDir":"/a","stateFile":"/s","replyTtl":"30h","blocks":[]}`), 0o600)
	if _, err := config.Load(path); err == nil {
		t.Fatal("unknown field / bad ttl accepted")
	}
	os.WriteFile(path, []byte(`{"signalCli":"unix:/x","archiveDir":"/a","stateFile":"/s","replyTtl":"30h"}`), 0o600)
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "replyTtl") {
		t.Fatalf("err = %v", err)
	}
}

// Two people both called "Sam Lee": the one you know (your contact name and
// nickname say so) and a stranger whose self-chosen profile name is the same.
// Names you set must win over names they set.
func TestNamesYouSetWin(t *testing.T) {
	h := setup(t)
	const known, stranger = "dddddddd-0000-0000-0000-000000000004", "eeeeeeee-0000-0000-0000-000000000005"
	profile := func(given, family string) *struct {
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} {
		return &struct {
			GivenName  string `json:"givenName"`
			FamilyName string `json:"familyName"`
		}{given, family}
	}
	h.f.contacts = append(h.f.contacts,
		signal.Contact{UUID: known, Number: "+15551114444", Name: "Sam Lee", GivenName: "Sam", FamilyName: "Lee",
			NickGivenName: "Sammy", NickFamilyName: "L", Profile: profile("Sam", "Lee")},
		signal.Contact{UUID: stranger, Number: "+15551115555", Profile: profile("Sam", "Lee")},
	)
	for _, who := range []string{`"Sam Lee"`, "sammy", `"Sammy L"`, "Sam"} {
		ans := h.cmd("/hush block " + who)
		h.mustContain(ans, "Sammy L") // shown by your nickname
		if i := h.d.findBlock(signal.Conversation{Recipient: known}); i < 0 {
			t.Fatalf("%s: didn't block the known Sam: %s", who, ans)
		}
		if i := h.d.findBlock(signal.Conversation{Recipient: stranger}); i >= 0 {
			t.Fatalf("%s: blocked the stranger", who)
		}
		h.cmd("/hush unblock " + who)
	}

	// Only profile names in play: genuinely ambiguous, and says how to pick.
	h.f.contacts = append(h.f.contacts,
		signal.Contact{UUID: "ffffffff-0000-0000-0000-000000000006", Number: "+15551116666", Profile: profile("Robin", "")},
		signal.Contact{UUID: "ffffffff-0000-0000-0000-000000000007", Number: "+15551117777", Profile: profile("Robin", "")},
	)
	h.mustContain(h.cmd("/hush block robin"), `"robin" is the profile name of 2`, "(…6666)", "(…7777)")
	h.mustContain(h.cmd("/hush block +15551117777"), "Blocked Robin")

	// Nickname also matches the block for unblock/read.
	h.cmd(`/hush block "Sam Lee"`)
	h.mustContain(h.cmd("/hush list"), "• Sammy L")
	h.mustContain(h.cmd("/hush unblock sammy"), "Unblocked Sammy L")
}

func TestContactJSON(t *testing.T) {
	// The field names signal-cli's listContacts actually uses.
	raw := `{"number":"+1","uuid":"u","name":"Sam Lee","givenName":"Sam","familyName":"Lee",
		"nickName":null,"nickGivenName":"Sammy","nickFamilyName":"L","profile":{"givenName":"S","familyName":""}}`
	var c signal.Contact
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.DisplayName() != "Sammy L" || c.NamesOf(signal.ContactName)[0] != "Sam Lee" || c.NamesOf(signal.ProfileName)[0] != "S" {
		t.Fatalf("contact = %+v", c)
	}
}
