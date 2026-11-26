package rws

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	acp "github.com/ironpark/acp-go"
)

const (
	// MaxMessageBytes matches acp-go's 50MB message ceiling so large image
	// attachments are not rejected by coder/websocket's 32KB default.
	MaxMessageBytes  = 50 << 20
	maxFrameOverhead = 1024

	DefaultDisconnectTTL   = 24 * time.Hour
	DefaultMaxBufferBytes  = 64 << 20
	DefaultMaxOfflineConns = 64
	DefaultPingInterval    = 15 * time.Second
	DefaultPingTimeout     = 15 * time.Second

	ackFlushDelay = 100 * time.Millisecond
	ackFlushBatch = 16
	inboundCap    = 256
)

var (
	errConnClosed    = errors.New("rws: connection closed")
	errSeqOutOfRange = errors.New("rws: peer ack out of range")
	errBufferFull    = errors.New("rws: unacknowledged buffer exceeded")
)

type queuedFrame struct {
	seq     uint64
	payload jsontext.Value
}

type socketLink struct {
	gen       uint64
	ws        *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	readDone  chan struct{} // closed once readLoop exits; guarantees single inbound writer
	startedAt time.Time
	lastRecv  atomic.Int64 // monotonic nanos since startedAt of last inbound frame
	pongOut   chan uint64  // nonces to reply with pong
}

// Conn is one logical ACP transport backed by zero or one physical WebSocket.
// It implements acp.Transport; physical disconnects and reconnects within
// DisconnectTTL are invisible to ReadMessage and WriteMessage callers.
type Conn struct {
	id      string
	srv     *Server
	inbound chan jsontext.Value
	closed  chan struct{}

	// attachMu serializes socket takeovers so a previous readLoop always exits
	// before the next socket snapshots recvSeq and installs itself.
	attachMu sync.Mutex

	mu             sync.Mutex
	isClosed       bool
	nextSendSeq    uint64
	peerAck        uint64
	sendQueue      []queuedFrame
	bufferedBytes  int
	recvSeq        uint64
	unackedRecv    int
	attachGen      uint64
	active         *socketLink
	disconnectedAt time.Time
	ttlTimer       *time.Timer

	wakeWrite chan struct{}
	wakeAck   chan struct{}
}

var _ acp.Transport = (*Conn)(nil)

func newConn(id string, srv *Server) *Conn {
	return &Conn{
		id:          id,
		srv:         srv,
		inbound:     make(chan jsontext.Value, inboundCap),
		closed:      make(chan struct{}),
		nextSendSeq: 1,
		wakeWrite:   make(chan struct{}, 1),
		wakeAck:     make(chan struct{}, 1),
	}
}

// ReadMessage returns the next ordered ACP JSON-RPC message, blocking across
// physical disconnects until a frame arrives, ctx ends, or the logical
// connection terminates.
func (c *Conn) ReadMessage(ctx context.Context) (jsontext.Value, error) {
	select {
	case msg := <-c.inbound:
		return msg, nil
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WriteMessage sequences data into the unacknowledged send buffer and wakes
// the active socket writer if one is attached. If the buffer exceeds
// MaxBufferBytes, the logical connection is terminated so slow or long-offline
// peers never exhaust server memory.
func (c *Conn) WriteMessage(_ context.Context, data jsontext.Value) error {
	payload := data.Clone()

	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return io.ErrClosedPipe
	}
	if c.srv.maxBufferBytes > 0 && c.bufferedBytes+len(payload) > c.srv.maxBufferBytes {
		old := c.terminateLocked()
		c.mu.Unlock()
		closeLink(old)
		c.srv.remove(c.id, c)
		return errBufferFull
	}
	seq := c.nextSendSeq
	c.nextSendSeq++
	c.sendQueue = append(c.sendQueue, queuedFrame{seq: seq, payload: payload})
	c.bufferedBytes += len(payload)
	c.mu.Unlock()

	signalChan(c.wakeWrite)
	return nil
}

// Close terminates the logical connection immediately. It closes the physical
// socket without sending "bye" so a server restart triggers client cold recovery
// rather than leaving the client permanently disconnected.
func (c *Conn) Close() error {
	c.terminate()
	return nil
}

// attach binds a freshly upgraded WebSocket to this logical connection.
// On resume, clientAck is validated against the retained unacknowledged buffer.
// Any stale half-open socket is closed and its reader drained before the new
// link snapshots recvSeq, preventing duplicate inbound delivery during takeover.
func (c *Conn) attach(ws *websocket.Conn, clientAck uint64, resumed bool) (*socketLink, []byte, error) {
	c.attachMu.Lock()
	defer c.attachMu.Unlock()

	// Validate before evicting any live socket, and stop an existing reader so
	// recvSeq is quiescent when we build the welcome frame.
	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil, nil, errConnClosed
	}
	if resumed && !c.validClientAckLocked(clientAck) {
		c.mu.Unlock()
		return nil, nil, errSeqOutOfRange
	}
	old := c.active
	c.mu.Unlock()

	if old != nil {
		closeLink(old)
		<-old.readDone
	}

	c.mu.Lock()
	if c.isClosed {
		c.mu.Unlock()
		return nil, nil, errConnClosed
	}
	if resumed {
		if !c.validClientAckLocked(clientAck) {
			c.mu.Unlock()
			return nil, nil, errSeqOutOfRange
		}
		c.pruneLocked(clientAck)
	}

	if c.ttlTimer != nil {
		c.ttlTimer.Stop()
		c.ttlTimer = nil
	}
	c.disconnectedAt = time.Time{}

	c.attachGen++
	ctx, cancel := context.WithCancel(c.srv.ctx)
	link := &socketLink{
		gen:       c.attachGen,
		ws:        ws,
		ctx:       ctx,
		cancel:    cancel,
		readDone:  make(chan struct{}),
		startedAt: time.Now(),
		pongOut:   make(chan uint64, 8),
	}
	c.active = link

	welcomeAck := c.recvSeq
	c.unackedRecv = 0
	ttlSecs := int64(c.srv.disconnectTTL / time.Second)
	c.mu.Unlock()

	welcome := encodeControl(header{
		Type:    typeWelcome,
		ID:      c.id,
		Ack:     welcomeAck,
		Resumed: resumed,
		TTL:     ttlSecs,
	})
	return link, welcome, nil
}

// validClientAckLocked reports whether clientAck lies in [oldest-1, nextSendSeq-1].
// c.mu must be held.
func (c *Conn) validClientAckLocked(clientAck uint64) bool {
	oldest := c.nextSendSeq
	if len(c.sendQueue) > 0 {
		oldest = c.sendQueue[0].seq
	}
	return clientAck >= oldest-1 && clientAck < c.nextSendSeq
}

// serveLink drives one attached WebSocket until it fails, is superseded, or
// the logical connection closes.
func (c *Conn) serveLink(link *socketLink, welcome []byte) {
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		c.writeLoop(link, welcome)
	}()
	c.readLoop(link)
	link.cancel()
	<-writeDone
	c.detach(link.gen)
}

func (c *Conn) writeLoop(link *socketLink, welcome []byte) {
	defer func() {
		link.cancel()
		link.ws.CloseNow()
	}()

	writeFrame := func(raw []byte) bool {
		ctx, cancel := context.WithTimeout(link.ctx, c.srv.writeTimeout())
		err := link.ws.Write(ctx, websocket.MessageText, raw)
		cancel()
		return err == nil
	}

	// welcome is always the first frame on a newly attached socket.
	if !writeFrame(welcome) {
		return
	}

	// sentSeq tracks the highest sequence sent over *this* physical socket, so
	// every unacknowledged frame in sendQueue is replayed in order before live
	// traffic resumes.
	var sentSeq uint64
	var ackTimer *time.Timer
	var ackTimerC <-chan time.Time
	stopAckTimer := func() {
		if ackTimer != nil {
			ackTimer.Stop()
			ackTimer = nil
			ackTimerC = nil
		}
	}
	defer stopAckTimer()

	var pingTimer *time.Timer
	var pingC <-chan time.Time
	if c.srv.pingInterval > 0 {
		pingTimer = time.NewTimer(c.srv.pingInterval)
		defer pingTimer.Stop()
		pingC = pingTimer.C
	}
	var pingNonce uint64
	var awaitingPing bool
	var pingSentAt time.Duration

	for {
		// 1. Prioritize pong replies for responsive client foreground probes.
		select {
		case nonce := <-link.pongOut:
			if !writeFrame(encodeControl(header{Type: typePong, Nonce: nonce})) {
				return
			}
			continue
		default:
		}

		// 2. Drain any queued messages newer than sentSeq.
		if frame, ack, ok := c.nextToSend(link, sentSeq); ok {
			stopAckTimer()
			if !writeFrame(encodeMsg(frame.seq, ack, frame.payload)) {
				return
			}
			sentSeq = frame.seq
			continue
		}

		// 3. Flush batched ACK immediately once threshold is reached.
		if ack, flushNow, needTimer := c.ackState(link); flushNow {
			stopAckTimer()
			if !writeFrame(encodeControl(header{Type: typeAck, Ack: ack})) {
				return
			}
			continue
		} else if needTimer && ackTimer == nil {
			ackTimer = time.NewTimer(ackFlushDelay)
			ackTimerC = ackTimer.C
		}

		select {
		case <-c.closed:
			return
		case <-link.ctx.Done():
			return
		case nonce := <-link.pongOut:
			if !writeFrame(encodeControl(header{Type: typePong, Nonce: nonce})) {
				return
			}
		case <-c.wakeWrite:
		case <-c.wakeAck:
		case <-ackTimerC:
			stopAckTimer()
			if ack, ok := c.takePendingAck(link); ok {
				if !writeFrame(encodeControl(header{Type: typeAck, Ack: ack})) {
					return
				}
			}
		case <-pingC:
			now := time.Since(link.startedAt)
			last := time.Duration(link.lastRecv.Load())
			if awaitingPing {
				if last >= pingSentAt {
					awaitingPing = false
				} else {
					return
				}
			}
			if idle := now - last; idle < c.srv.pingInterval {
				pingTimer.Reset(c.srv.pingInterval - idle)
			} else {
				pingNonce++
				pingSentAt = now
				if !writeFrame(encodeControl(header{Type: typePing, Nonce: pingNonce})) {
					return
				}
				awaitingPing = true
				pingTimer.Reset(c.srv.pingTimeout)
			}
		}
	}
}

func (c *Conn) readLoop(link *socketLink) {
	defer close(link.readDone)
	defer func() {
		link.cancel()
		link.ws.CloseNow()
	}()

	for {
		kind, raw, err := link.ws.Read(link.ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			continue
		}
		link.lastRecv.Store(int64(time.Since(link.startedAt)))

		h, payload, err := decodeFrame(raw)
		if err != nil {
			return
		}
		switch h.Type {
		case typeMsg:
			if payload == nil {
				return
			}
			c.mu.Lock()
			if h.Ack >= c.nextSendSeq {
				c.mu.Unlock()
				return
			}
			c.pruneLocked(h.Ack)
			expected := c.recvSeq + 1
			c.mu.Unlock()

			if h.Seq < expected {
				// Duplicate retransmission: drop payload, prompt ACK so peer
				// can release its send queue.
				c.markDuplicateAck()
				continue
			}
			if h.Seq > expected {
				// Sequence gap on a single ordered socket: force reconnect to resync.
				return
			}
			// Deliver before advancing recvSeq so a disconnect while blocked on
			// backpressure never acknowledges an undelivered message.
			select {
			case c.inbound <- payload:
			case <-link.ctx.Done():
				return
			case <-c.closed:
				return
			}
			c.mu.Lock()
			c.recvSeq = h.Seq
			c.unackedRecv++
			c.mu.Unlock()
			signalChan(c.wakeAck)

		case typeAck:
			c.mu.Lock()
			if h.Ack >= c.nextSendSeq {
				c.mu.Unlock()
				return
			}
			c.pruneLocked(h.Ack)
			c.mu.Unlock()

		case typePing:
			select {
			case link.pongOut <- h.Nonce:
			default:
			}

		case typePong:
			// lastRecv was already updated above.

		case typeBye:
			c.terminate()
			return
		}
	}
}

func (c *Conn) nextToSend(link *socketLink, afterSeq uint64) (queuedFrame, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != link || len(c.sendQueue) == 0 {
		return queuedFrame{}, 0, false
	}
	// Sequences in sendQueue are contiguous: sendQueue[i].seq == head + i.
	head := c.sendQueue[0].seq
	idx := 0
	if afterSeq >= head {
		offset := afterSeq - head + 1
		if offset >= uint64(len(c.sendQueue)) {
			return queuedFrame{}, 0, false
		}
		idx = int(offset)
	}
	ack := c.recvSeq
	c.unackedRecv = 0
	return c.sendQueue[idx], ack, true
}

func (c *Conn) ackState(link *socketLink) (ack uint64, flushNow bool, needTimer bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != link || c.unackedRecv == 0 {
		return 0, false, false
	}
	if c.unackedRecv >= ackFlushBatch {
		ack = c.recvSeq
		c.unackedRecv = 0
		return ack, true, false
	}
	return 0, false, true
}

func (c *Conn) takePendingAck(link *socketLink) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != link || c.unackedRecv == 0 {
		return 0, false
	}
	ack := c.recvSeq
	c.unackedRecv = 0
	return ack, true
}

func (c *Conn) markDuplicateAck() {
	c.mu.Lock()
	c.unackedRecv = ackFlushBatch
	c.mu.Unlock()
	signalChan(c.wakeAck)
}

// pruneLocked discards acknowledged frames from the head of sendQueue.
// c.mu must be held.
func (c *Conn) pruneLocked(ack uint64) {
	if ack >= c.nextSendSeq {
		ack = c.nextSendSeq - 1
	}
	if ack <= c.peerAck {
		return
	}
	c.peerAck = ack
	drop := 0
	for drop < len(c.sendQueue) && c.sendQueue[drop].seq <= ack {
		c.bufferedBytes -= len(c.sendQueue[drop].payload)
		c.sendQueue[drop].payload = nil
		drop++
	}
	if drop == 0 {
		return
	}
	remaining := len(c.sendQueue) - drop
	if remaining == 0 {
		c.sendQueue = nil
		c.bufferedBytes = 0
		return
	}
	// Compact when over half the backing array has been consumed.
	if drop >= cap(c.sendQueue)/2 {
		next := make([]queuedFrame, remaining)
		copy(next, c.sendQueue[drop:])
		c.sendQueue = next
	} else {
		c.sendQueue = c.sendQueue[drop:]
	}
}

func (c *Conn) detach(gen uint64) {
	c.mu.Lock()
	if c.isClosed || c.attachGen != gen || c.active == nil {
		c.mu.Unlock()
		return
	}
	c.active = nil
	if ttl := c.srv.disconnectTTL; ttl <= 0 {
		c.terminateLocked()
		c.mu.Unlock()
		c.srv.remove(c.id, c)
		return
	} else {
		c.disconnectedAt = time.Now()
		c.ttlTimer = time.AfterFunc(ttl, func() { c.expireIfDetached(gen) })
	}
	c.mu.Unlock()

	c.srv.evictExcessOffline()
}

func (c *Conn) expireIfDetached(gen uint64) {
	c.mu.Lock()
	if c.isClosed || c.attachGen != gen || c.active != nil {
		c.mu.Unlock()
		return
	}
	old := c.terminateLocked()
	c.mu.Unlock()

	closeLink(old)
	c.srv.remove(c.id, c)
}

func (c *Conn) offlineSince() (time.Time, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed || c.active != nil || c.disconnectedAt.IsZero() {
		return time.Time{}, 0, false
	}
	return c.disconnectedAt, c.attachGen, true
}

func (c *Conn) terminate() {
	c.mu.Lock()
	old := c.terminateLocked()
	c.mu.Unlock()

	closeLink(old)
	c.srv.remove(c.id, c)
}

func (c *Conn) terminateLocked() *socketLink {
	if c.isClosed {
		return nil
	}
	c.isClosed = true
	if c.ttlTimer != nil {
		c.ttlTimer.Stop()
		c.ttlTimer = nil
	}
	old := c.active
	c.active = nil
	c.sendQueue = nil
	c.bufferedBytes = 0
	close(c.closed)
	return old
}

func closeLink(link *socketLink) {
	if link == nil {
		return
	}
	link.cancel()
	link.ws.CloseNow()
}

func signalChan(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
