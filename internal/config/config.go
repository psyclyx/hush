// Package config loads hush's JSON config: plumbing and reply policy. Blocks
// themselves are runtime state, managed from Note to Self.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"text/template"
	"time"
)

// DefaultTemplate is the auto-reply. Fields: .Name .Reason .Until (empty when
// indefinite) .Escape (empty when disabled) .ReplyTTL.
const DefaultTemplate = `[automatic reply] I've paused this conversation{{if .Until}} until {{.Until}}{{end}}{{if .Reason}}: {{.Reason}}{{end}}.

Your messages are saved and I'll read them later, but they won't reach me before then.{{if .Escape}} If something can't wait, start your message with "{{.Escape}}" and it will come straight through.{{end}}

This note deletes itself after {{.ReplyTTL}} without new messages.`

// DefaultEscapeTemplate is sent when the escape word opens a window.
// Fields: .Name .Window.
const DefaultEscapeTemplate = `[automatic reply] Got it — your messages are coming through to me now. This stays open until {{.Window}} pass without a message, then the pause resumes and this note deletes itself.`

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: want a string like \"5m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

type Config struct {
	// Account is this signal-cli account's number (E.164). Optional when
	// signal-cli has exactly one account.
	Account string `json:"account"`
	// SignalCli is the daemon's JSON-RPC address: unix:/path or tcp:host:port.
	SignalCli string `json:"signalCli"`
	// SignalCliAttachments is signal-cli's attachment download directory
	// (<config>/attachments). hush empties it, so only archived (blocked)
	// messages' attachments persist. Optional.
	SignalCliAttachments string `json:"signalCliAttachments"`
	// ArchiveDir receives every message from a blocked conversation.
	ArchiveDir string `json:"archiveDir"`
	// StateFile holds blocks and pending deletions.
	StateFile string `json:"stateFile"`

	// ReplyTTL is how long an auto-reply stays up after their last message.
	ReplyTTL Duration `json:"replyTtl"`
	// CommandTTL is how long /hush commands and their answers stay in Note
	// to Self before being cleared.
	CommandTTL Duration `json:"commandTtl"`
	// KeepCommands disables that clearing.
	KeepCommands bool `json:"keepCommands"`
	// Escape is the escape-hatch prefix; "" disables it. Default "URGENT".
	Escape *string `json:"escape"`
	// EscapeWindow: after the escape word, their messages pass through until
	// this long goes by with no message from them or you. Default 2m.
	EscapeWindow Duration `json:"escapeWindow"`
	// Timezone for rendering and parsing times (IANA name); default local.
	Timezone string `json:"timezone"`
	// Template overrides DefaultTemplate.
	Template string `json:"template"`
	// EscapeTemplate overrides DefaultEscapeTemplate.
	EscapeTemplate string `json:"escapeTemplate"`

	Location   *time.Location     `json:"-"`
	Tmpl       *template.Template `json:"-"`
	EscapeTmpl *template.Template `json:"-"`
}

// EscapeWord resolves the escape-hatch prefix.
func (c *Config) EscapeWord() string {
	if c.Escape == nil {
		return "URGENT"
	}
	return *c.Escape
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &Config{}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) finish() error {
	var errs []error
	for name, v := range map[string]string{"signalCli": c.SignalCli, "archiveDir": c.ArchiveDir, "stateFile": c.StateFile} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	def := func(d *Duration, v time.Duration) {
		if d.Duration == 0 {
			d.Duration = v
		}
	}
	def(&c.ReplyTTL, 5*time.Minute)
	def(&c.CommandTTL, 10*time.Minute)
	def(&c.EscapeWindow, 2*time.Minute)
	// Remote delete only works within a day of sending.
	for name, d := range map[string]time.Duration{"replyTtl": c.ReplyTTL.Duration, "escapeWindow": c.EscapeWindow.Duration} {
		if d >= 23*time.Hour {
			errs = append(errs, fmt.Errorf("%s must be under 23h (Signal refuses remote deletes after 24h)", name))
		}
	}
	c.Location = time.Local
	if c.Timezone != "" {
		loc, err := time.LoadLocation(c.Timezone)
		if err != nil {
			errs = append(errs, fmt.Errorf("timezone: %w", err))
		} else {
			c.Location = loc
		}
	}
	parse := func(name, text, def string) *template.Template {
		if text == "" {
			text = def
		}
		t, err := template.New(name).Parse(text)
		if err != nil {
			errs = append(errs, err)
		}
		return t
	}
	c.Tmpl = parse("template", c.Template, DefaultTemplate)
	c.EscapeTmpl = parse("escapeTemplate", c.EscapeTemplate, DefaultEscapeTemplate)
	return errors.Join(errs...)
}
