package softblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/psyclyx/hush/internal/signal"
)

// Commands are Note to Self messages starting with this prefix. Answers start
// with "[hush]", so hush never reads its own output as a command.
const commandPrefix = "/hush"

const help = `/hush block <who> [3d | 12h | until 2026-12-01 [18:00]] [reason…]
/hush unblock <who>
/hush list
/hush read <who> [count]

<who> is a contact or group name (quote names with spaces), a +number, or a uuid.`

func (d *Daemon) noteToSelf(ctx context.Context, sm *signal.SentMessage) {
	text := strings.TrimSpace(sm.Message)
	if !hasPrefixFold(text, commandPrefix) {
		return
	}
	rest := text[len(commandPrefix):]
	if rest != "" && rest[0] != ' ' && rest[0] != '\n' {
		return // e.g. "/hushed", not ours
	}
	args, err := tokenize(rest)
	var answer string
	if err == nil {
		answer, err = d.command(ctx, args)
	}
	if err != nil {
		answer = "⚠ " + err.Error()
	}
	slog.Info("command handled", "ok", err == nil)
	self := signal.Conversation{NoteToSelf: true}
	if !d.cfg.KeepCommands {
		d.schedule(self, sm.Timestamp, d.now().Add(d.cfg.CommandTTL.Duration), true)
	}
	d.tellSelf(ctx, answer, !d.cfg.KeepCommands)
	d.save()
}

// tellSelf posts to Note to Self, optionally clearing it after CommandTTL.
func (d *Daemon) tellSelf(ctx context.Context, text string, ephemeral bool) {
	self := signal.Conversation{NoteToSelf: true}
	ts, err := d.sig.Send(ctx, self, "[hush] "+text)
	if err != nil {
		slog.Error("note to self", "err", scrub(err))
		return
	}
	if ephemeral {
		d.schedule(self, ts, d.now().Add(d.cfg.CommandTTL.Duration), true)
	}
}

func (d *Daemon) command(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return help, nil
	}
	verb, args := strings.ToLower(args[0]), args[1:]
	switch verb {
	case "help":
		return help, nil
	case "list", "ls":
		return d.cmdList(), nil
	case "block":
		return d.cmdBlock(ctx, args)
	case "unblock":
		return d.cmdUnblock(ctx, args)
	case "read":
		return d.cmdRead(ctx, args)
	}
	return "", fmt.Errorf("unknown command %q\n\n%s", verb, help)
}

func (d *Daemon) cmdBlock(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("block who?\n\n" + help)
	}
	t, err := d.resolve(ctx, args[0])
	if err != nil {
		return "", err
	}
	args = args[1:]
	now := d.now()
	var until *time.Time
	switch {
	case len(args) > 0 && strings.EqualFold(args[0], "until"):
		u, n, err := d.parseUntil(args[1:])
		if err != nil {
			return "", err
		}
		if !u.After(now) {
			return "", fmt.Errorf("%s is in the past", d.fmtTime(u))
		}
		until, args = &u, args[1+n:]
	case len(args) > 0:
		if dur, ok := parseDuration(args[0]); ok {
			u := now.Add(dur)
			until, args = &u, args[1:]
		} else if strings.EqualFold(args[0], "indefinitely") || strings.EqualFold(args[0], "forever") {
			args = args[1:]
		}
	}
	b := Block{Name: t.name, Conversation: t.conv, Number: t.number, Reason: strings.Join(args, " "), Since: now, Until: until}

	verb := "Blocked"
	if i := d.findBlock(t.conv); i >= 0 {
		verb = "Updated block on"
		b.Since = d.state.Blocks[i].Since
		d.state.Blocks[i] = b
	} else {
		d.state.Blocks = append(d.state.Blocks, b)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s %s.", verb, b.Name, d.describeUntil(&b))
	if b.Reason != "" {
		fmt.Fprintf(&sb, " Reason: %s.", b.Reason)
	}
	sb.WriteString(" Their messages will be saved and hidden; they'll get an auto-reply.")
	if e := d.cfg.EscapeWord(); e != "" {
		fmt.Fprintf(&sb, " Escape word: %s.", e)
	}
	return sb.String(), nil
}

func (d *Daemon) cmdUnblock(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("usage: /hush unblock <who>")
	}
	i, err := d.blockFor(ctx, args[0])
	if err != nil {
		return "", err
	}
	if i < 0 {
		return "", fmt.Errorf("%s isn't blocked", args[0])
	}
	b := d.state.Blocks[i]
	d.state.Blocks = slices.Delete(d.state.Blocks, i, i+1)
	return fmt.Sprintf("Unblocked %s. Saved messages: /hush read %s", b.Name, quoteName(b.Name)), nil
}

func (d *Daemon) cmdList() string {
	if len(d.state.Blocks) == 0 {
		return "Nothing is blocked."
	}
	var sb strings.Builder
	for i := range d.state.Blocks {
		b := &d.state.Blocks[i]
		fmt.Fprintf(&sb, "• %s — %s", b.Name, d.describeUntil(b))
		if b.Reason != "" {
			fmt.Fprintf(&sb, " (%s)", b.Reason)
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

func (d *Daemon) cmdRead(ctx context.Context, args []string) (string, error) {
	if len(args) < 1 || len(args) > 2 {
		return "", errors.New("usage: /hush read <who> [count]")
	}
	n := 10
	if len(args) == 2 {
		v, err := strconv.Atoi(args[1])
		if err != nil || v < 1 {
			return "", fmt.Errorf("count %q: want a positive number", args[1])
		}
		n = min(v, 100)
	}
	// Read what was saved under a current block, else whatever the name
	// resolves to (a past block's archive is keyed the same way).
	var key signal.Conversation
	name := args[0]
	if i, err := d.blockFor(ctx, args[0]); err == nil && i >= 0 {
		key, name = d.state.Blocks[i].Conversation, d.state.Blocks[i].Name
	} else {
		t, err := d.resolve(ctx, args[0])
		if err != nil {
			return "", err
		}
		key, name = t.conv, t.name
	}
	recs, err := d.archive.Load(key)
	if err != nil {
		return "", fmt.Errorf("reading archive: %v", scrub(err))
	}
	if len(recs) == 0 {
		return fmt.Sprintf("Nothing saved from %s.", name), nil
	}
	shown := recs[max(0, len(recs)-n):]
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — last %d of %d saved:\n", name, len(shown), len(recs))
	for _, r := range shown {
		sb.WriteString("\n")
		sb.WriteString(d.formatRecord(&r, key.GroupID != ""))
	}
	return sb.String(), nil
}

func (d *Daemon) formatRecord(r *Record, group bool) string {
	var env signal.Envelope
	_ = json.Unmarshal(r.Envelope, &env)
	dm, edited := env.DataMessage, false
	if env.EditMessage != nil {
		dm, edited = env.EditMessage.DataMessage, true
	}
	var sb strings.Builder
	sb.WriteString(r.ReceivedAt.In(d.cfg.Location).Format("Jan 2 3:04 PM"))
	if group && env.SourceName != "" {
		fmt.Fprintf(&sb, " · %s", env.SourceName)
	}
	sb.WriteString(": ")
	if edited {
		sb.WriteString("(edit) ")
	}
	if r.Escaped {
		sb.WriteString("(escaped) ")
	}
	if dm != nil {
		sb.WriteString(dm.Message)
		if k := len(dm.Attachments); k > 0 {
			fmt.Fprintf(&sb, " [%s]", plural(k, "attachment"))
		}
		if dm.HasSticker() {
			sb.WriteString(" [sticker]")
		}
	}
	return sb.String()
}

// target is what a <who> argument resolved to.
type target struct {
	name   string
	conv   signal.Conversation
	number string
}

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	numberRe = regexp.MustCompile(`^\+[0-9]{6,15}$`)
)

// resolve turns a name, number or uuid into a conversation, using signal-cli's
// contacts and groups.
//
// Names are tried exact (case-insensitive) before prefix, and within each, by
// kind: your nicknames, then your contact names and group names, then
// people's self-chosen profile names, then usernames/numbers. The first kind
// with any match decides, so a name you gave someone wins over a stranger who
// picked the same profile name.
func (d *Daemon) resolve(ctx context.Context, who string) (target, error) {
	contacts, err := d.sig.Contacts(ctx)
	if err != nil {
		return target{}, fmt.Errorf("listing contacts: %v", scrub(err))
	}
	fromContact := func(c *signal.Contact) target {
		return target{name: c.DisplayName(), conv: signal.Conversation{Recipient: c.ID()}, number: c.Number}
	}
	switch {
	case numberRe.MatchString(who):
		for i := range contacts {
			if contacts[i].Number == who {
				return fromContact(&contacts[i]), nil
			}
		}
		return target{name: who, conv: signal.Conversation{Recipient: who}, number: who}, nil
	case uuidRe.MatchString(who):
		for i := range contacts {
			if strings.EqualFold(contacts[i].UUID, who) {
				return fromContact(&contacts[i]), nil
			}
		}
		return target{name: who, conv: signal.Conversation{Recipient: strings.ToLower(who)}}, nil
	}

	groups, err := d.sig.Groups(ctx)
	if err != nil {
		return target{}, fmt.Errorf("listing groups: %v", scrub(err))
	}
	for _, match := range []func(name string) bool{
		func(n string) bool { return strings.EqualFold(n, who) },
		func(n string) bool { return hasPrefixFold(n, who) },
	} {
		for kind := range signal.NumNameKinds {
			var found []target
			for i := range contacts {
				if slices.ContainsFunc(contacts[i].NamesOf(kind), match) {
					found = append(found, fromContact(&contacts[i]))
				}
			}
			if kind == signal.ContactName {
				for _, g := range groups {
					if g.IsMember && g.Name != "" && match(g.Name) {
						found = append(found, target{name: g.Name, conv: signal.Conversation{GroupID: g.ID}})
					}
				}
			}
			switch len(found) {
			case 0:
				continue
			case 1:
				return found[0], nil
			default:
				return target{}, ambiguous(who, kind, found)
			}
		}
	}
	return target{}, fmt.Errorf("no contact or group matches %q", who)
}

// ambiguous explains a name collision with enough to tell the matches apart:
// the last digits of each number, and how to pick one exactly.
func ambiguous(who string, kind signal.NameKind, found []target) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%q is the %s of %d:", who, kind, len(found))
	for _, f := range found[:min(len(found), 8)] {
		sb.WriteString("\n• " + f.name)
		switch {
		case f.conv.GroupID != "":
			sb.WriteString(" (group)")
		case len(f.number) > 4:
			fmt.Fprintf(&sb, " (…%s)", f.number[len(f.number)-4:])
		}
	}
	if len(found) > 8 {
		fmt.Fprintf(&sb, "\n…and %d more", len(found)-8)
	}
	sb.WriteString("\nUse their full name or nickname, or their +number.")
	return errors.New(sb.String())
}

// blockFor finds the current block <who> refers to: by block name first (no
// lookup needed), else by resolving it. -1 when not blocked.
func (d *Daemon) blockFor(ctx context.Context, who string) (int, error) {
	for i := range d.state.Blocks {
		if strings.EqualFold(d.state.Blocks[i].Name, who) {
			return i, nil
		}
	}
	t, err := d.resolve(ctx, who)
	if err != nil {
		return -1, err
	}
	return d.findBlock(t.conv), nil
}

func (d *Daemon) findBlock(conv signal.Conversation) int {
	return slices.IndexFunc(d.state.Blocks, func(b Block) bool { return b.Conversation == conv })
}

func (d *Daemon) describeUntil(b *Block) string {
	if b.Until == nil {
		return "indefinitely"
	}
	return "until " + d.fmtTime(*b.Until)
}

func (d *Daemon) fmtTime(t time.Time) string {
	return t.In(d.cfg.Location).Format("Mon Jan 2, 3:04 PM")
}

// parseUntil reads "2026-12-01" or "2026-12-01 18:00" (or RFC 3339), in the
// configured timezone, returning how many args it used.
func (d *Daemon) parseUntil(args []string) (time.Time, int, error) {
	if len(args) == 0 {
		return time.Time{}, 0, errors.New("until when? e.g. until 2026-12-01 18:00")
	}
	if t, err := time.Parse(time.RFC3339, args[0]); err == nil {
		return t, 1, nil
	}
	if len(args) > 1 {
		if t, err := time.ParseInLocation("2006-01-02 15:04", args[0]+" "+args[1], d.cfg.Location); err == nil {
			return t, 2, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, args[0], d.cfg.Location); err == nil {
			return t, 1, nil
		}
	}
	return time.Time{}, 0, fmt.Errorf("can't read %q as a date; try 2026-12-01 or 2026-12-01 18:00", args[0])
}

var durationRe = regexp.MustCompile(`^(\d+[wdhm])+$`)
var durationPart = regexp.MustCompile(`(\d+)([wdhm])`)

// parseDuration reads "3d", "1w", "2d12h", "90m".
func parseDuration(s string) (time.Duration, bool) {
	s = strings.ToLower(s)
	if !durationRe.MatchString(s) {
		return 0, false
	}
	unit := map[string]time.Duration{"w": 7 * 24 * time.Hour, "d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}
	var total time.Duration
	for _, m := range durationPart.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		total += time.Duration(n) * unit[m[2]]
	}
	return total, total > 0
}

// tokenize splits on whitespace, honoring "double" or 'single' quotes (and
// the curly quotes phones substitute). A quote only opens at the start of a
// word, and a single quote only closes at the end of one, so apostrophes in
// a reason ("I'm away", "friends' trip") stay literal.
func tokenize(s string) ([]string, error) {
	s = strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'").Replace(s)
	rs := []rune(s)
	space := func(i int) bool { return i >= len(rs) || rs[i] == ' ' || rs[i] == '\t' || rs[i] == '\n' }
	var out []string
	var cur strings.Builder
	var quote rune
	inToken := false
	for i, r := range rs {
		switch {
		case quote != 0:
			if r == quote && (quote == '"' || space(i+1)) {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case (r == '"' || r == '\'') && !inToken:
			quote, inToken = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inToken {
				out = append(out, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote")
	}
	if inToken {
		out = append(out, cur.String())
	}
	return out, nil
}

func quoteName(n string) string {
	if strings.ContainsAny(n, " \t") {
		return `"` + n + `"`
	}
	return n
}

var scrubRe = regexp.MustCompile(`\+[0-9]{6,15}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[A-Za-z0-9+/]{40,}={0,2}`)

// scrub strips numbers, uuids and group ids from an error before it is
// logged; signal-cli errors sometimes name who they are about.
func scrub(err error) string {
	if err == nil {
		return ""
	}
	return scrubRe.ReplaceAllString(err.Error(), "<redacted>")
}
