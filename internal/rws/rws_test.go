package rws

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	acp "github.com/ironpark/acp-go"
)

type testPeer struct {
	ws *websocket.Conn
}

func dialPeer(t *testing.T, url string, id string, ack uint64) (*testPeer, header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(url, "http")
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	hello := encodeControl(header{Type: typeHello, ID: id, Ack: ack})
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_, raw, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read handshake reply: %v", err)
	}
	h, _, err := decodeFrame(raw)
	if err != nil {
		t.Fatalf("decode handshake reply: %v", err)
	}
	return &testPeer{ws: ws}, h
}

func (p *testPeer) sendMsg(t *testing.T, seq, ack uint64, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.ws.Write(ctx, websocket.MessageText, encodeMsg(seq, ack, jsontext.Value(payload))); err != nil {
		t.Fatalf("send msg %d: %v", seq, err)
	}
}

func (p *testPeer) sendControl(t *testing.T, h header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw := encodeControl(h)
	if err := p.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("send control %s: %v", h.Type, err)
	}
}

func (p *testPeer) readFrame(t *testing.T) (header, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, raw, err := p.ws.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	h, payload, err := decodeFrame(raw)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return h, string(payload)
}

func (p *testPeer) readMsg(t *testing.T) (header, string) {
	t.Helper()
	for {
		h, payload := p.readFrame(t)
		if h.Type == typeMsg {
			return h, payload
		}
	}
}

func TestReconnectReplaysUnackedMessagesTransparently(t *testing.T) {
	var serveCalls atomic.Int32
	transports := make(chan acp.Transport, 2)
	received := make(chan string, 16)

	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		serveCalls.Add(1)
		transports <- tr
		for {
			msg, err := tr.ReadMessage(ctx)
			if err != nil {
				return err
			}
			received <- string(msg)
		}
	}, WithDisconnectTTL(time.Minute), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, welcome := dialPeer(t, httpSrv.URL, "", 0)
	if welcome.Type != typeWelcome || welcome.ID == "" || welcome.Resumed || welcome.TTL != 60 {
		t.Fatalf("unexpected welcome: %+v", welcome)
	}
	tr := <-transports

	// Exchange 2 messages in each direction before disconnecting.
	p1.sendMsg(t, 1, 0, `{"client":1}`)
	if got := <-received; got != `{"client":1}` {
		t.Fatalf("got %s", got)
	}
	for i := 1; i <= 3; i++ {
		if err := tr.WriteMessage(context.Background(), jsontext.Value(fmt.Sprintf(`{"server":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 3; i++ {
		h, payload := p1.readMsg(t)
		if h.Seq != uint64(i) || payload != fmt.Sprintf(`{"server":%d}`, i) {
			t.Fatalf("got seq=%d payload=%s", h.Seq, payload)
		}
	}

	// Simulate abrupt physical disconnect after client received up to seq 3.
	p1.ws.CloseNow()
	time.Sleep(50 * time.Millisecond)

	// Server keeps producing messages while client is offline.
	for i := 4; i <= 8; i++ {
		if err := tr.WriteMessage(context.Background(), jsontext.Value(fmt.Sprintf(`{"server":%d}`, i))); err != nil {
			t.Fatalf("offline write %d: %v", i, err)
		}
	}

	// Client reconnects acknowledging seq 3.
	p2, w2 := dialPeer(t, httpSrv.URL, welcome.ID, 3)
	if w2.Type != typeWelcome || !w2.Resumed || w2.Ack != 1 {
		t.Fatalf("unexpected resume welcome: %+v", w2)
	}
	for i := 4; i <= 8; i++ {
		h, payload := p2.readMsg(t)
		if h.Seq != uint64(i) || payload != fmt.Sprintf(`{"server":%d}`, i) {
			t.Fatalf("replay got seq=%d payload=%s, want %d", h.Seq, payload, i)
		}
	}

	// Retransmitted client seq 1 is deduplicated; seq 2 is delivered.
	p2.sendMsg(t, 1, 8, `{"client":1}`)
	p2.sendMsg(t, 2, 8, `{"client":2}`)
	if got := <-received; got != `{"client":2}` {
		t.Fatalf("expected deduplicated client 2, got %s", got)
	}
	if n := serveCalls.Load(); n != 1 {
		t.Fatalf("serve called %d times, want 1 (transparent to ACP)", n)
	}
}

func TestTakeoverClosesHalfOpenOldSocket(t *testing.T) {
	transports := make(chan acp.Transport, 1)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		transports <- tr
		_, err := tr.ReadMessage(ctx)
		return err
	}, WithDisconnectTTL(150*time.Millisecond), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, w1 := dialPeer(t, httpSrv.URL, "", 0)
	tr := <-transports

	// Open second socket with same conn ID while p1 is still open (half-open scenario).
	p2, w2 := dialPeer(t, httpSrv.URL, w1.ID, 0)
	if w2.Type != typeWelcome || !w2.Resumed {
		t.Fatalf("unexpected resume: %+v", w2)
	}

	// Old socket must be closed by server.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := p1.ws.Read(ctx); err == nil {
		t.Fatal("expected old socket to be closed on takeover")
	}

	// Wait longer than DisconnectTTL to verify old socket's detach did NOT start expiry.
	time.Sleep(250 * time.Millisecond)
	if err := tr.WriteMessage(context.Background(), jsontext.Value(`{"alive":true}`)); err != nil {
		t.Fatalf("connection expired after takeover: %v", err)
	}
	h, payload := p2.readMsg(t)
	if h.Seq != 1 || payload != `{"alive":true}` {
		t.Fatalf("unexpected frame: %+v %s", h, payload)
	}
}

func TestDisconnectTTLExpiresLogicalConnection(t *testing.T) {
	ended := make(chan error, 1)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		_, err := tr.ReadMessage(ctx)
		ended <- err
		return err
	}, WithDisconnectTTL(80*time.Millisecond), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, w1 := dialPeer(t, httpSrv.URL, "", 0)
	p1.ws.CloseNow()

	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF on TTL expiry, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logical connection did not expire after TTL")
	}

	_, reply := dialPeer(t, httpSrv.URL, w1.ID, 0)
	if reply.Type != typeExpired {
		t.Fatalf("expected expired reply, got %+v", reply)
	}
}

func TestBufferLimitTerminatesOfflineConnection(t *testing.T) {
	transports := make(chan acp.Transport, 1)
	ended := make(chan error, 1)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		transports <- tr
		_, err := tr.ReadMessage(ctx)
		ended <- err
		return err
	}, WithDisconnectTTL(time.Minute), WithMaxBufferBytes(256), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, w1 := dialPeer(t, httpSrv.URL, "", 0)
	tr := <-transports
	p1.ws.CloseNow()
	time.Sleep(30 * time.Millisecond)

	big := jsontext.Value(`{"data":"` + strings.Repeat("x", 300) + `"}`)
	if err := tr.WriteMessage(context.Background(), big); !errors.Is(err, errBufferFull) {
		t.Fatalf("expected errBufferFull, got %v", err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadMessage did not unblock after buffer overflow")
	}
	_, reply := dialPeer(t, httpSrv.URL, w1.ID, 0)
	if reply.Type != typeExpired {
		t.Fatalf("expected expired after buffer overflow, got %+v", reply)
	}
}

func TestMaxOfflineConnsEvictsOldest(t *testing.T) {
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		_, err := tr.ReadMessage(ctx)
		return err
	}, WithDisconnectTTL(time.Minute), WithMaxOfflineConns(2), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	ids := make([]string, 3)
	for i := range 3 {
		p, w := dialPeer(t, httpSrv.URL, "", 0)
		ids[i] = w.ID
		p.ws.CloseNow()
		time.Sleep(25 * time.Millisecond)
	}

	_, r0 := dialPeer(t, httpSrv.URL, ids[0], 0)
	if r0.Type != typeExpired {
		t.Fatalf("oldest offline conn should be evicted, got %+v", r0)
	}
	_, r1 := dialPeer(t, httpSrv.URL, ids[1], 0)
	if r1.Type != typeWelcome || !r1.Resumed {
		t.Fatalf("second conn should survive, got %+v", r1)
	}
	_, r2 := dialPeer(t, httpSrv.URL, ids[2], 0)
	if r2.Type != typeWelcome || !r2.Resumed {
		t.Fatalf("newest conn should survive, got %+v", r2)
	}
}

func TestByeTerminatesImmediately(t *testing.T) {
	ended := make(chan error, 1)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		_, err := tr.ReadMessage(ctx)
		ended <- err
		return err
	}, WithDisconnectTTL(time.Hour), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, w1 := dialPeer(t, httpSrv.URL, "", 0)
	p1.sendControl(t, header{Type: typeBye})

	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF after bye, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bye did not terminate logical connection")
	}

	_, reply := dialPeer(t, httpSrv.URL, w1.ID, 0)
	if reply.Type != typeExpired {
		t.Fatalf("expected expired after bye, got %+v", reply)
	}
}

func TestPingPongAndServerPingTimeout(t *testing.T) {
	var mu sync.Mutex
	var conns []*Conn
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		mu.Lock()
		conns = append(conns, tr.(*Conn))
		mu.Unlock()
		_, err := tr.ReadMessage(ctx)
		return err
	}, WithDisconnectTTL(time.Minute), WithPing(60*time.Millisecond, 60*time.Millisecond))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p, w := dialPeer(t, httpSrv.URL, "", 0)
	p.sendControl(t, header{Type: typePing, Nonce: 7})
	h, _ := p.readFrame(t)
	if h.Type != typePong || h.Nonce != 7 {
		t.Fatalf("expected pong 7, got %+v", h)
	}

	// Stop replying to server pings: physical socket must drop, logical conn survives.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		_, _, err := p.ws.Read(ctx)
		if err != nil {
			break
		}
	}
	_, w2 := dialPeer(t, httpSrv.URL, w.ID, 0)
	if w2.Type != typeWelcome || !w2.Resumed {
		t.Fatalf("logical conn should survive ping timeout, got %+v", w2)
	}
}

func TestRapidTakeoversDeliverEachMessageExactlyOnce(t *testing.T) {
	const total = 100
	received := make(chan string, total*2)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		for {
			msg, err := tr.ReadMessage(ctx)
			if err != nil {
				return err
			}
			received <- string(msg)
		}
	}, WithDisconnectTTL(time.Minute), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p, w := dialPeer(t, httpSrv.URL, "", 0)
	var sentSeq uint64
	for round := range 5 {
		for range 20 {
			sentSeq++
			p.sendMsg(t, sentSeq, 0, fmt.Sprintf(`{"n":%d}`, sentSeq))
		}
		// Immediately take over with a new socket without closing the previous one.
		next, nw := dialPeer(t, httpSrv.URL, w.ID, 0)
		if nw.Type != typeWelcome || !nw.Resumed {
			t.Fatalf("round %d: unexpected welcome %+v", round, nw)
		}
		// Replay anything the server has not yet acknowledged.
		for seq := nw.Ack + 1; seq <= sentSeq; seq++ {
			next.sendMsg(t, seq, 0, fmt.Sprintf(`{"n":%d}`, seq))
		}
		p = next
	}

	for i := 1; i <= total; i++ {
		select {
		case got := <-received:
			want := fmt.Sprintf(`{"n":%d}`, i)
			if got != want {
				t.Fatalf("message %d: got %s, want %s", i, got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for message %d/%d", i, total)
		}
	}
	select {
	case extra := <-received:
		t.Fatalf("duplicate message delivered: %s", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestZeroDisconnectTTLExpiresImmediatelyOnDisconnect(t *testing.T) {
	ended := make(chan error, 1)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		_, err := tr.ReadMessage(ctx)
		ended <- err
		return err
	}, WithDisconnectTTL(0), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, w1 := dialPeer(t, httpSrv.URL, "", 0)
	p1.ws.CloseNow()

	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF with zero TTL, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("zero-TTL connection did not terminate on disconnect")
	}

	_, reply := dialPeer(t, httpSrv.URL, w1.ID, 0)
	if reply.Type != typeExpired {
		t.Fatalf("expected expired reply, got %+v", reply)
	}
}

func TestOutOfRangeAckDropsSocketWithoutPoisoningPeerAck(t *testing.T) {
	transports := make(chan acp.Transport, 1)
	srv := NewServer(func(ctx context.Context, tr acp.Transport) error {
		transports <- tr
		_, err := tr.ReadMessage(ctx)
		return err
	}, WithDisconnectTTL(time.Minute), WithPing(0, 0))
	t.Cleanup(func() { _ = srv.Close() })
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)

	p1, w1 := dialPeer(t, httpSrv.URL, "", 0)
	tr := <-transports

	// Send an impossible future ack; server must drop the socket without
	// advancing peerAck past nextSendSeq.
	p1.sendControl(t, header{Type: typeAck, Ack: 9999})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := p1.ws.Read(ctx); err == nil {
		t.Fatal("expected socket to close on out-of-range ack")
	}

	if err := tr.WriteMessage(context.Background(), jsontext.Value(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	p2, w2 := dialPeer(t, httpSrv.URL, w1.ID, 0)
	if w2.Type != typeWelcome || !w2.Resumed {
		t.Fatalf("expected resumed welcome, got %+v", w2)
	}
	h, payload := p2.readMsg(t)
	if h.Seq != 1 || payload != `{"ok":true}` {
		t.Fatalf("expected seq 1 after reconnect, got seq=%d payload=%s", h.Seq, payload)
	}
	// Valid ack for seq 1 must still be accepted and prune sendQueue.
	p2.sendControl(t, header{Type: typeAck, Ack: 1})
	time.Sleep(30 * time.Millisecond)
	conn := tr.(*Conn)
	conn.mu.Lock()
	remaining := len(conn.sendQueue)
	conn.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected empty sendQueue after valid ack, got %d", remaining)
	}
}
