// Package signal is a minimal JSON-RPC client for `signal-cli daemon`.
//
// One connection carries both directions: requests are matched to responses
// by id, and `receive` notifications are delivered in order on a channel.
package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Conversation names a 1:1 chat (Recipient: number or uuid), a group
// (GroupID: base64), or Note to Self. Exactly one is set.
type Conversation struct {
	Recipient  string `json:"recipient,omitempty"`
	GroupID    string `json:"groupId,omitempty"`
	NoteToSelf bool   `json:"noteToSelf,omitempty"`
}

func (c Conversation) String() string {
	if c.NoteToSelf {
		return "note-to-self"
	}
	if c.GroupID != "" {
		return "group:" + c.GroupID
	}
	return c.Recipient
}

// params adds the conversation's addressing to p, in the shape signal-cli's
// send/remoteDelete/getAttachment commands take.
func (c Conversation) params(p map[string]any, recipientKey string, list bool) {
	if c.NoteToSelf {
		p["noteToSelf"] = true
		return
	}
	if c.GroupID != "" {
		p["groupId"] = c.GroupID
		return
	}
	if list {
		p[recipientKey] = []string{c.Recipient}
	} else {
		p[recipientKey] = c.Recipient
	}
}

type Envelope struct {
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	Timestamp    int64        `json:"timestamp"`
	DataMessage  *DataMessage `json:"dataMessage"`
	EditMessage  *EditMessage `json:"editMessage"`
	SyncMessage  *SyncMessage `json:"syncMessage"`
}

// SyncMessage is what this account's other devices did, e.g. a message you
// sent from your phone.
type SyncMessage struct {
	SentMessage *SentMessage `json:"sentMessage"`
}

type SentMessage struct {
	DestinationNumber string `json:"destinationNumber"`
	DestinationUUID   string `json:"destinationUuid"`
	DataMessage
}

// NoteToSelf returns the text of a message you sent yourself from another
// device, if this envelope is one.
func (e *Envelope) NoteToSelf() (*SentMessage, bool) {
	if e.SyncMessage == nil || e.SyncMessage.SentMessage == nil {
		return nil, false
	}
	sm := e.SyncMessage.SentMessage
	if sm.GroupInfo != nil {
		return nil, false
	}
	// Sync messages come from our own account, so the source is us.
	self := (sm.DestinationUUID != "" && sm.DestinationUUID == e.SourceUUID) ||
		(sm.DestinationNumber != "" && sm.DestinationNumber == e.SourceNumber)
	return sm, self
}

// Source is the sender's most stable identifier.
func (e *Envelope) Source() string {
	if e.SourceUUID != "" {
		return e.SourceUUID
	}
	return e.SourceNumber
}

type DataMessage struct {
	Timestamp          int64           `json:"timestamp"`
	Message            string          `json:"message"`
	IsExpirationUpdate bool            `json:"isExpirationUpdate"`
	GroupInfo          *GroupInfo      `json:"groupInfo"`
	Attachments        []Attachment    `json:"attachments"`
	Sticker            json.RawMessage `json:"sticker"`
	Contacts           json.RawMessage `json:"contacts"`
	PollCreate         json.RawMessage `json:"pollCreate"`
}

// HasContent reports whether the message is something that shows up as a
// message bubble (and so can be deleted), as opposed to a reaction, receipt,
// remote delete, timer change or other control message.
func (d *DataMessage) HasContent() bool {
	return d.Message != "" || len(d.Attachments) > 0 || d.HasSticker() ||
		present(d.Contacts) || present(d.PollCreate)
}

func (d *DataMessage) HasSticker() bool {
	return present(d.Sticker)
}

type EditMessage struct {
	TargetSentTimestamp int64        `json:"targetSentTimestamp"`
	DataMessage         *DataMessage `json:"dataMessage"`
}

type GroupInfo struct {
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"`
	Type      string `json:"type"`
}

type Attachment struct {
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
	ID          string `json:"id"`
	Size        int64  `json:"size"`
}

// Received is one `receive` notification: the decoded envelope plus the raw
// JSON, which is what gets archived.
type Received struct {
	Envelope Envelope
	Raw      json.RawMessage
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("signal-cli: %s (code %d)", e.Message, e.Code) }

type frame struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type response struct {
	result json.RawMessage
	err    error
}

// Client keeps one connection to signal-cli open, redialing when it drops.
type Client struct {
	network, addr string
	account       string
	self          string

	mu      sync.Mutex
	conn    net.Conn
	nextID  int64
	pending map[int64]chan response

	// Received carries incoming envelopes, in arrival order.
	Received chan Received
}

// New makes a client for addr, given as "unix:/path" or "tcp:host:port".
func New(addr, account string) (*Client, error) {
	network, rest, ok := strings.Cut(addr, ":")
	if !ok || (network != "unix" && network != "tcp") {
		return nil, fmt.Errorf("signal-cli address %q: want unix:/path or tcp:host:port", addr)
	}
	return &Client{
		network:  network,
		addr:     rest,
		account:  account,
		pending:  map[int64]chan response{},
		Received: make(chan Received, 64),
	}, nil
}

// Run dials and reads until ctx is done, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		var d net.Dialer
		conn, err := d.DialContext(ctx, c.network, c.addr)
		if err != nil {
			slog.Warn("dial signal-cli", "addr", c.addr, "err", err, "retry", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		slog.Info("connected to signal-cli", "addr", c.addr)
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		err = c.read(ctx, conn)
		stop()
		conn.Close()
		c.mu.Lock()
		c.conn = nil
		for id, ch := range c.pending {
			ch <- response{err: errors.New("connection to signal-cli lost")}
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if ctx.Err() == nil {
			slog.Warn("signal-cli connection lost", "err", err)
		}
	}
}

func (c *Client) read(ctx context.Context, conn net.Conn) error {
	// json.Decoder has no line-length limit; attachments come back base64'd.
	dec := json.NewDecoder(conn)
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			return err
		}
		switch {
		case f.ID != nil:
			c.mu.Lock()
			ch := c.pending[*f.ID]
			delete(c.pending, *f.ID)
			c.mu.Unlock()
			if ch != nil {
				r := response{result: f.Result}
				if f.Error != nil {
					r.err = f.Error
				}
				ch <- r
			}
		case f.Method == "receive":
			var p struct {
				Envelope json.RawMessage `json:"envelope"`
			}
			if err := json.Unmarshal(f.Params, &p); err != nil {
				slog.Warn("bad receive notification", "err", err)
				continue
			}
			var r Received
			r.Raw = p.Envelope
			if err := json.Unmarshal(p.Envelope, &r.Envelope); err != nil {
				slog.Warn("bad envelope", "err", err)
				continue
			}
			select {
			case c.Received <- r:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (c *Client) call(ctx context.Context, method string, params map[string]any, out any) error {
	if c.account != "" {
		params["account"] = c.account
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return errors.New("not connected to signal-cli")
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err == nil {
		_, err = c.conn.Write(append(req, '\n'))
	}
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", method, r.err)
		}
		if out != nil {
			return json.Unmarshal(r.result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// Send posts text to the conversation and returns its sent timestamp.
func (c *Client) Send(ctx context.Context, conv Conversation, text string) (int64, error) {
	p := map[string]any{"message": text}
	conv.params(p, "recipient", true)
	var res struct {
		Timestamp int64 `json:"timestamp"`
		Results   []struct {
			Type string `json:"type"`
		} `json:"results"`
	}
	if err := c.call(ctx, "send", p, &res); err != nil {
		return 0, err
	}
	for _, r := range res.Results {
		if r.Type != "SUCCESS" {
			slog.Warn("send partially failed", "conversation", conv, "result", r.Type)
		}
	}
	return res.Timestamp, nil
}

// RemoteDelete deletes one of our own sent messages for everyone.
func (c *Client) RemoteDelete(ctx context.Context, conv Conversation, ts int64) error {
	p := map[string]any{"targetTimestamp": ts}
	conv.params(p, "recipient", true)
	return c.call(ctx, "remoteDelete", p, nil)
}

// DeleteForMe removes messages from this account's other devices only. It
// needs signal-cli built with hush's delete-for-me patch.
func (c *Client) DeleteForMe(ctx context.Context, conv Conversation, author string, ts ...int64) error {
	p := map[string]any{"targetAuthor": author, "targetTimestamp": ts}
	conv.params(p, "recipient", false)
	return c.call(ctx, "deleteForMe", p, nil)
}

// Attachment fetches a downloaded attachment's bytes.
func (c *Client) Attachment(ctx context.Context, conv Conversation, id string) ([]byte, error) {
	p := map[string]any{"id": id}
	conv.params(p, "recipient", false)
	var res struct {
		Data string `json:"data"`
	}
	if err := c.call(ctx, "getAttachment", p, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

// Contact is one entry of signal-cli's listContacts. Names come from three
// places: the nickname you set in Signal (nickGivenName/nickFamilyName), your
// address-book name (name/givenName/familyName, synced from your phone), and
// the profile name the person chose for themselves.
type Contact struct {
	Number         string `json:"number"`
	UUID           string `json:"uuid"`
	Username       string `json:"username"`
	Name           string `json:"name"`
	GivenName      string `json:"givenName"`
	FamilyName     string `json:"familyName"`
	NickName       string `json:"nickName"` // legacy; usually empty
	NickGivenName  string `json:"nickGivenName"`
	NickFamilyName string `json:"nickFamilyName"`
	Profile        *struct {
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"profile"`
}

// NameKind says where a name came from, most trusted first: names you chose
// beat names the person chose for themselves.
type NameKind int

const (
	Nickname NameKind = iota
	ContactName
	ProfileName
	OtherName // username, number
	NumNameKinds
)

func (k NameKind) String() string {
	return [...]string{"nickname", "contact name", "profile name", "username/number"}[k]
}

// NamesOf lists the contact's names of one kind, fullest first.
func (c *Contact) NamesOf(k NameKind) []string {
	join := func(a, b string) string { return strings.TrimSpace(a + " " + b) }
	var names []string
	switch k {
	case Nickname:
		names = []string{join(c.NickGivenName, c.NickFamilyName), c.NickGivenName, c.NickName}
	case ContactName:
		names = []string{c.Name, join(c.GivenName, c.FamilyName), c.GivenName}
	case ProfileName:
		if c.Profile != nil {
			names = []string{join(c.Profile.GivenName, c.Profile.FamilyName), c.Profile.GivenName}
		}
	case OtherName:
		names = []string{c.Username, c.Number}
	}
	var out []string
	for _, n := range names {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// DisplayName is what you'd call the contact: nickname, else contact name,
// else profile name, else username/number.
func (c *Contact) DisplayName() string {
	for k := range NumNameKinds {
		if names := c.NamesOf(k); len(names) > 0 {
			return names[0]
		}
	}
	return c.ID()
}

// ID is the contact's most stable identifier.
func (c *Contact) ID() string {
	if c.UUID != "" {
		return c.UUID
	}
	return c.Number
}

type Group struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsMember bool   `json:"isMember"`
}

func (c *Client) Contacts(ctx context.Context) ([]Contact, error) {
	var out []Contact
	return out, c.call(ctx, "listContacts", map[string]any{}, &out)
}

func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var out []Group
	return out, c.call(ctx, "listGroups", map[string]any{}, &out)
}

// Self is this account's number: the configured one, else signal-cli's only
// account.
func (c *Client) Self(ctx context.Context) (string, error) {
	if c.account != "" {
		return c.account, nil
	}
	c.mu.Lock()
	self := c.self
	c.mu.Unlock()
	if self != "" {
		return self, nil
	}
	var accounts []struct {
		Number string `json:"number"`
	}
	if err := c.call(ctx, "listAccounts", map[string]any{}, &accounts); err != nil {
		return "", err
	}
	if len(accounts) != 1 {
		return "", fmt.Errorf("signal-cli has %d accounts; set account in the config", len(accounts))
	}
	c.mu.Lock()
	c.self = accounts[0].Number
	c.mu.Unlock()
	return accounts[0].Number, nil
}

// present reports whether an optional JSON field holds a value.
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "[]" && s != "{}"
}
