package signal

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeDaemon answers requests on a unix socket, recording them, and can push
// notifications.
func fakeDaemon(t *testing.T) (addr string, reqs chan map[string]any, push chan string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	reqs = make(chan map[string]any, 16)
	push = make(chan string, 16)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			for p := range push {
				conn.Write([]byte(p + "\n"))
			}
		}()
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req map[string]any
			json.Unmarshal(sc.Bytes(), &req)
			reqs <- req
			var result string
			switch req["method"] {
			case "send":
				result = `{"timestamp":4242,"results":[{"type":"SUCCESS"}]}`
			case "listAccounts":
				result = `[{"number":"+15559999999","aci":"x"}]`
			case "getAttachment":
				result = `{"data":"aGVsbG8="}`
			default:
				result = `{}`
			}
			id, _ := json.Marshal(req["id"])
			conn.Write([]byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":` + result + "}\n"))
		}
	}()
	return "unix:" + sock, reqs, push
}

func TestWire(t *testing.T) {
	addr, reqs, push := fakeDaemon(t)
	c, err := New(addr, "+15550000000")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	push <- `{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"sourceUuid":"u1","sourceNumber":"+1","timestamp":7,"dataMessage":{"timestamp":7,"message":"hi","groupInfo":{"groupId":"g=="}}},"account":"+15550000000"}}`
	var r Received
	select {
	case r = <-c.Received:
	case <-ctx.Done():
		t.Fatal("no notification")
	}
	if r.Envelope.Source() != "u1" || r.Envelope.DataMessage.GroupInfo.GroupID != "g==" || len(r.Raw) == 0 {
		t.Fatalf("received %+v", r)
	}

	dm := Conversation{Recipient: "u1"}
	grp := Conversation{GroupID: "g=="}

	ts, err := c.Send(ctx, dm, "text")
	if err != nil || ts != 4242 {
		t.Fatalf("send = %d, %v", ts, err)
	}
	expect(t, <-reqs, "send", `{"account":"+15550000000","message":"text","recipient":["u1"]}`)

	if err := c.RemoteDelete(ctx, grp, 4242); err != nil {
		t.Fatal(err)
	}
	expect(t, <-reqs, "remoteDelete", `{"account":"+15550000000","groupId":"g==","targetTimestamp":4242}`)

	if err := c.DeleteForMe(ctx, dm, "u1", 7, 8); err != nil {
		t.Fatal(err)
	}
	expect(t, <-reqs, "deleteForMe", `{"account":"+15550000000","recipient":"u1","targetAuthor":"u1","targetTimestamp":[7,8]}`)

	data, err := c.Attachment(ctx, grp, "a.jpg")
	if err != nil || string(data) != "hello" {
		t.Fatalf("attachment = %q, %v", data, err)
	}
	expect(t, <-reqs, "getAttachment", `{"account":"+15550000000","groupId":"g==","id":"a.jpg"}`)
}

func expect(t *testing.T, req map[string]any, method, params string) {
	t.Helper()
	got, _ := json.Marshal(req["params"])
	if req["method"] != method || string(got) != params {
		t.Fatalf("got %v %s\nwant %s %s", req["method"], got, method, params)
	}
}

func TestSelfDiscovery(t *testing.T) {
	addr, reqs, _ := fakeDaemon(t)
	c, err := New(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)
	var self string
	for ctx.Err() == nil { // until connected
		if self, err = c.Self(ctx); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if self != "+15559999999" {
		t.Fatalf("self = %q, %v", self, err)
	}
	expect(t, <-reqs, "listAccounts", `{}`)
	// Cached, and no account param when none is configured.
	if s, _ := c.Self(ctx); s != self {
		t.Fatal("not cached")
	}
	if err := c.RemoteDelete(ctx, Conversation{NoteToSelf: true}, 1); err != nil {
		t.Fatal(err)
	}
	expect(t, <-reqs, "remoteDelete", `{"noteToSelf":true,"targetTimestamp":1}`)
}
