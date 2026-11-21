package softblock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/psyclyx/hush/internal/signal"
)

// Archive lays messages out as <dir>/<conversation>/messages.jsonl, with
// attachments beside them in attachments/.
type Archive struct{ Dir string }

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

func (a *Archive) convDir(c signal.Conversation) string {
	name := "dm-" + c.Recipient
	if c.GroupID != "" {
		name = "group-" + c.GroupID
	}
	return filepath.Join(a.Dir, unsafe.ReplaceAllString(name, "_"))
}

// Save writes the attachments (fetched from conv, where the message arrived),
// then appends the record under key, fsyncing both.
func (a *Archive) Save(ctx context.Context, sig Signal, key, conv signal.Conversation, rec Record, atts []signal.Attachment) error {
	dir := a.convDir(key)
	attDir := filepath.Join(dir, "attachments")
	if err := os.MkdirAll(attDir, 0o700); err != nil {
		return err
	}
	for _, at := range atts {
		if at.ID == "" {
			continue
		}
		data, err := sig.Attachment(ctx, conv, at.ID)
		if err != nil {
			return fmt.Errorf("attachment %s: %w", at.ID, err)
		}
		name := unsafe.ReplaceAllString(at.ID, "_")
		if err := writeFileSync(filepath.Join(attDir, name), data); err != nil {
			return err
		}
		rec.Attachments = append(rec.Attachments, filepath.Join("attachments", name))
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// PendingDelete is an auto-reply waiting to be deleted.
type PendingDelete struct {
	Conversation signal.Conversation `json:"conversation"`
	Timestamp    int64               `json:"timestamp"`
	SentAt       time.Time           `json:"sentAt"`
	DeleteAt     time.Time           `json:"deleteAt"`
	// LocalOnly: only on our devices (Note to Self), so no remote delete.
	LocalOnly bool `json:"localOnly,omitempty"`
	// Digest identifies an auto-reply's text, to tell when it's gone stale.
	Digest string `json:"digest,omitempty"`
	// Kind of notice to them: "reply" or "escape" (empty in old state: reply).
	Kind string `json:"kind,omitempty"`
}

// State survives restarts: blocks and deletions still owed.
type State struct {
	Blocks  []Block         `json:"blocks"`
	Pending []PendingDelete `json:"pending"`
}

func LoadState(path string) (*State, error) {
	st := &State{}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return st, nil
}

func (s *State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, raw); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads a conversation's archived records, oldest first.
func (a *Archive) Load(key signal.Conversation) ([]Record, error) {
	raw, err := os.ReadFile(filepath.Join(a.convDir(key), "messages.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []Record
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return recs, nil
}

func (p *PendingDelete) noticeKind() string {
	if p.Kind == "" {
		return noticeReply
	}
	return p.Kind
}
