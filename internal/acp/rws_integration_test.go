package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/config"
	"github.com/coxlong/harness-acp/internal/registry"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type fakeLiveHandle struct {
	id        string
	events    chan backend.Event
	decisions chan backend.Decision
}

func (h *fakeLiveHandle) NativeID() string              { return h.id }
func (h *fakeLiveHandle) Events() <-chan backend.Event  { return h.events }
func (h *fakeLiveHandle) Options() backend.Options      { return backend.Options{} }
func (h *fakeLiveHandle) Cancel(context.Context) error  { return nil }
func (h *fakeLiveHandle) Close(context.Context) error   { close(h.events); return nil }
func (h *fakeLiveHandle) Send(context.Context, []acp2.ContentBlock) (acp2.MessageID, error) {
	return "m1", nil
}
func (h *fakeLiveHandle) SetConfigOption(context.Context, string, any) ([]acp2.SessionConfigOption, error) {
	return nil, nil
}
func (h *fakeLiveHandle) Respond(_ string, d backend.Decision) error {
	h.decisions <- d
	return nil
}

type fakeLiveBackend struct {
	backend.Backend
	handle *fakeLiveHandle
}

func (b *fakeLiveBackend) Name() string { return "claude" }
func (b *fakeLiveBackend) NewSession(context.Context, backend.NewSessionParams) (backend.SessionHandle, error) {
	return b.handle, nil
}

func TestReliableWSPreservesSessionAndPermissionAcrossDisconnect(t *testing.T) {
	handle := &fakeLiveHandle{
		id:        "s-live",
		events:    make(chan backend.Event, 16),
		decisions: make(chan backend.Decision, 4),
	}
	cfg := config.Default()
	cfg.Token = testBearerToken
	cfg.Transport.DisconnectTTL = time.Minute
	srv := NewServer(cfg, map[string]backend.Backend{"claude": &fakeLiveBackend{handle: handle}}, session.New(), registry.New())
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/acp/claude"

	dial := func(id string, ack uint64) (*websocket.Conn, map[string]any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + testBearerToken}},
		})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { ws.CloseNow() })
		hello, _ := json.Marshal(map[string]any{"t": "hello", "id": id, "a": ack})
		if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
			t.Fatalf("hello: %v", err)
		}
		_, raw, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("welcome: %v", err)
		}
		var h map[string]any
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatalf("decode welcome: %v", err)
		}
		return ws, h
	}

	readMsg := func(ws *websocket.Conn) (uint64, map[string]any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			_, raw, err := ws.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			head := raw
			var payload []byte
			if nl := bytes.IndexByte(raw, '\n'); nl >= 0 {
				head = raw[:nl]
				payload = raw[nl+1:]
			}
			var h struct {
				T string `json:"t"`
				S uint64 `json:"s"`
			}
			if err := json.Unmarshal(head, &h); err != nil {
				t.Fatalf("decode head: %v", err)
			}
			if h.T != "msg" {
				continue
			}
			var rpc map[string]any
			if err := json.Unmarshal(payload, &rpc); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			return h.S, rpc
		}
	}

	sendMsg := func(ws *websocket.Conn, seq, ack uint64, rpc any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body, _ := json.Marshal(rpc)
		frame := append([]byte(fmt.Sprintf(`{"t":"msg","s":%d,"a":%d}`+"\n", seq, ack)), body...)
		if err := ws.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatalf("write seq %d: %v", seq, err)
		}
	}

	ws1, w1 := dial("", 0)
	connID, _ := w1["id"].(string)
	if connID == "" {
		t.Fatalf("missing conn id: %+v", w1)
	}

	// initialize + session/new on ws1
	sendMsg(ws1, 1, 0, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 2}})
	seq, _ := readMsg(ws1)
	sendMsg(ws1, 2, seq, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": "/work"}})
	seq, _ = readMsg(ws1)

	// Drop physical socket.
	ws1.CloseNow()
	time.Sleep(50 * time.Millisecond)

	// While offline, backend emits an update AND a permission request.
	handle.events <- backend.Event{
		Kind: backend.EventUpdate,
		Update: schema.NewSessionUpdate(schema.SessionUpdateAgentMessageChunk{
			MessageID: "a1", Content: acp2.TextBlock("offline-chunk"),
		}),
	}
	handle.events <- backend.Event{
		Kind: backend.EventPermission,
		Permission: backend.PermissionRequest{
			ReqID: "perm-1", ToolCallID: "tc-1", ToolName: "Bash", Title: "Run tests",
		},
	}
	time.Sleep(50 * time.Millisecond)

	// Permission must NOT have been rejected while disconnected.
	select {
	case d := <-handle.decisions:
		t.Fatalf("permission decided while client was offline within TTL: %+v", d)
	default:
	}

	// Reconnect on ws2: NO initialize and NO session/resume needed!
	ws2, w2 := dial(connID, seq)
	if w2["t"] != "welcome" || w2["resumed"] != true {
		t.Fatalf("expected resumed welcome, got %+v", w2)
	}

	var gotUpdate bool
	var permReqID any
	var allowOptID string
	for !gotUpdate || permReqID == nil {
		var rpc map[string]any
		seq, rpc = readMsg(ws2)
		switch rpc["method"] {
		case "session/update":
			gotUpdate = true
		case "session/request_permission":
			permReqID = rpc["id"]
			params, _ := rpc["params"].(map[string]any)
			opts, _ := params["options"].([]any)
			for _, raw := range opts {
				o, _ := raw.(map[string]any)
				if o["kind"] == "allow_once" {
					allowOptID, _ = o["optionId"].(string)
				}
			}
		}
	}
	if allowOptID == "" {
		t.Fatal("missing allow_once option")
	}

	// Answer the permission on ws2; backend must receive Allow: true.
	sendMsg(ws2, 3, seq, map[string]any{
		"jsonrpc": "2.0",
		"id":      permReqID,
		"result":  map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": allowOptID}},
	})
	select {
	case d := <-handle.decisions:
		if !d.Allow {
			t.Fatalf("expected Allow=true, got %+v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backend did not receive permission decision after reconnect")
	}
}
