package rws

import (
	"context"
	"crypto/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	acp "github.com/ironpark/acp-go"
)

const handshakeTimeout = 10 * time.Second

// Option configures a Server.
type Option func(*Server)

func WithOrigins(patterns ...string) Option {
	return func(s *Server) { s.origins = append(s.origins, patterns...) }
}

func WithDisconnectTTL(d time.Duration) Option {
	return func(s *Server) { s.disconnectTTL = d }
}

func WithMaxBufferBytes(n int) Option {
	return func(s *Server) { s.maxBufferBytes = n }
}

func WithMaxOfflineConns(n int) Option {
	return func(s *Server) { s.maxOfflineConns = n }
}

func WithPing(interval, timeout time.Duration) Option {
	return func(s *Server) {
		s.pingInterval = interval
		s.pingTimeout = timeout
	}
}

func WithErrorHandler(fn func(error)) Option {
	return func(s *Server) { s.onError = fn }
}

// Server serves reliable, resumable WebSocket connections for one ACP endpoint.
// serve is called once per logical connection and continues running across
// physical disconnects until the logical connection closes or expires.
type Server struct {
	serve           func(ctx context.Context, t acp.Transport) error
	origins         []string
	disconnectTTL   time.Duration
	maxBufferBytes  int
	maxOfflineConns int
	pingInterval    time.Duration
	pingTimeout     time.Duration
	onError         func(error)

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	conns   map[string]*Conn
	running sync.WaitGroup
}

func NewServer(serve func(ctx context.Context, t acp.Transport) error, opts ...Option) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		serve:           serve,
		disconnectTTL:   DefaultDisconnectTTL,
		maxBufferBytes:  DefaultMaxBufferBytes,
		maxOfflineConns: DefaultMaxOfflineConns,
		pingInterval:    DefaultPingInterval,
		pingTimeout:     DefaultPingTimeout,
		ctx:             ctx,
		cancel:          cancel,
		conns:           make(map[string]*Conn),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.pingInterval > 0 && s.pingTimeout <= 0 {
		s.pingTimeout = s.pingInterval
	}
	return s
}

func (s *Server) writeTimeout() time.Duration {
	if s.pingTimeout > 0 {
		return s.pingTimeout
	}
	return 15 * time.Second
}

// ServeHTTP accepts a WebSocket upgrade, runs the single-frame hello/welcome
// handshake, and attaches the socket to a new or existing logical connection.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.origins})
	if err != nil {
		return
	}
	ws.SetReadLimit(MaxMessageBytes + maxFrameOverhead)

	hctx, hcancel := context.WithTimeout(r.Context(), handshakeTimeout)
	kind, raw, err := ws.Read(hctx)
	hcancel()
	if err != nil || kind != websocket.MessageText {
		ws.CloseNow()
		return
	}
	h, _, err := decodeFrame(raw)
	if err != nil || h.Type != typeHello {
		ws.CloseNow()
		return
	}

	if h.ID != "" {
		s.handleResume(ws, h)
		return
	}
	s.handleNew(ws, r)
}

func (s *Server) handleNew(ws *websocket.Conn, r *http.Request) {
	id := rand.Text()
	conn := newConn(id, s)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close(websocket.StatusGoingAway, "server closed")
		return
	}
	s.conns[id] = conn
	s.running.Add(1)
	s.mu.Unlock()

	link, welcome, err := conn.attach(ws, 0, false)
	if err != nil {
		s.remove(id, conn)
		s.running.Done()
		ws.CloseNow()
		return
	}

	// Preserve request values (e.g. auth context) without inheriting the
	// HTTP upgrade request cancellation.
	connCtx, connCancel := context.WithCancel(context.WithoutCancel(r.Context()))
	unlink := context.AfterFunc(s.ctx, connCancel)
	go func() {
		defer s.running.Done()
		defer unlink()
		defer connCancel()
		defer conn.terminate()
		if err := s.serve(connCtx, conn); err != nil && connCtx.Err() == nil && s.onError != nil {
			s.onError(err)
		}
	}()

	conn.serveLink(link, welcome)
}

func (s *Server) handleResume(ws *websocket.Conn, h header) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ws.Close(websocket.StatusGoingAway, "server closed")
		return
	}
	conn := s.conns[h.ID]
	s.mu.Unlock()

	if conn == nil {
		s.sendExpired(ws, "unknown_connection")
		return
	}
	link, welcome, err := conn.attach(ws, h.Ack, true)
	if err != nil {
		reason := "connection_expired"
		if err == errSeqOutOfRange {
			reason = "sequence_out_of_range"
		}
		s.sendExpired(ws, reason)
		return
	}
	conn.serveLink(link, welcome)
}

func (s *Server) sendExpired(ws *websocket.Conn, reason string) {
	raw := encodeControl(header{Type: typeExpired, Reason: reason})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = ws.Write(ctx, websocket.MessageText, raw)
	cancel()
	_ = ws.Close(websocket.StatusNormalClosure, reason)
}

func (s *Server) remove(id string, c *Conn) {
	s.mu.Lock()
	if s.conns[id] == c {
		delete(s.conns, id)
	}
	s.mu.Unlock()
}

func (s *Server) evictExcessOffline() {
	if s.maxOfflineConns <= 0 {
		return
	}
	s.mu.Lock()
	all := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()

	type offline struct {
		conn *Conn
		at   time.Time
		gen  uint64
	}
	var list []offline
	for _, c := range all {
		if at, gen, ok := c.offlineSince(); ok {
			list = append(list, offline{conn: c, at: at, gen: gen})
		}
	}
	if len(list) <= s.maxOfflineConns {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	for _, victim := range list[:len(list)-s.maxOfflineConns] {
		victim.conn.expireIfDetached(victim.gen)
	}
}

// Close ends every logical and physical connection immediately without
// waiting for serve goroutines.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	all := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		all = append(all, c)
	}
	s.conns = make(map[string]*Conn)
	s.mu.Unlock()

	for _, c := range all {
		c.terminate()
	}
	return nil
}

// Shutdown closes all connections and waits for serve callbacks to return or
// ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	_ = s.Close()
	done := make(chan struct{})
	go func() {
		s.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
