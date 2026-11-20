package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
)

// ErrSessionNotFound is returned when a session ID is unknown.
var ErrSessionNotFound = errors.New("session not found")

// ErrTurnConflict is returned when a concurrent turn is already running.
var ErrTurnConflict = errors.New("turn already in progress")

// entry holds live state for one session.
type entry struct {
	mu        sync.Mutex
	handle    backend.SessionHandle
	agentName string

	// ring buffer for disconnect-replay (_harness/seq cursor)
	buf     []seqEvent
	bufHead int    // next write position (wraps)
	bufSeq  uint64 // monotone counter of next event
	bufCap  int

	// subscribers: active connections watching this session
	subs   map[uint64]chan backend.Event
	subSeq uint64

	// state is the session's latest foreground state with its background work,
	// for the snapshot a resuming connection gets. The zero value is idle.
	state backend.TurnEvent

	// single-turn mutex: for claude one-process-at-a-time
	turnMu sync.Mutex
	idle   time.Time
}

type seqEvent struct {
	seq uint64
	ev  backend.Event
}

// Registry holds all active sessions.
type Registry struct {
	mu       sync.RWMutex
	sessions map[string]*entry
	bufCap   int
}

// New creates a Registry with the given ring buffer capacity.
func New(bufCap int) *Registry {
	if bufCap <= 0 {
		bufCap = 500
	}
	return &Registry{
		sessions: make(map[string]*entry),
		bufCap:   bufCap,
	}
}

// Register adds a new session handle to the registry.
func (r *Registry) Register(id, agentName string, h backend.SessionHandle) {
	e := &entry{
		handle:    h,
		agentName: agentName,
		buf:       make([]seqEvent, r.bufCap),
		bufCap:    r.bufCap,
		subs:      make(map[uint64]chan backend.Event),
		idle:      time.Now(),
	}
	r.mu.Lock()
	r.sessions[id] = e
	r.mu.Unlock()
	go r.pump(id, e)
}

// pump reads from the handle's Events channel, appends to the ring buffer,
// and broadcasts to all current subscribers.
func (r *Registry) pump(id string, e *entry) {
	for ev := range e.handle.Events() {
		e.mu.Lock()
		if ev.Kind == backend.EventTurnState && ev.Session == "" {
			// A snapshot is not a completion: it keeps the state and the
			// background work, and no stop reason or error.
			e.state = backend.TurnEvent{State: ev.Turn.State, Background: ev.Turn.Background}
		}
		seq := e.bufSeq
		e.bufSeq++
		slot := e.bufHead % e.bufCap
		e.buf[slot] = seqEvent{seq: seq, ev: ev}
		e.bufHead++
		// broadcast to subscribers (non-blocking: drop slow ones)
		for subID, ch := range e.subs {
			select {
			case ch <- ev:
			default:
				// slow subscriber: close its channel so the connection handler detects it
				close(ch)
				delete(e.subs, subID)
			}
		}
		e.mu.Unlock()
	}
	// handle closed: remove from registry
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
}

// Subscribe attaches a new event channel to the session.
// Returns the channel and an unsubscribe func.
func (r *Registry) Subscribe(id string) (<-chan backend.Event, func(), error) {
	e := r.entry(id)
	if e == nil {
		return nil, nil, ErrSessionNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, unsub := e.subscribeLocked()
	return ch, unsub, nil
}

func (e *entry) subscribeLocked() (<-chan backend.Event, func()) {
	ch := make(chan backend.Event, 256)
	sid := e.subSeq
	e.subSeq++
	e.subs[sid] = ch
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if _, ok := e.subs[sid]; ok {
			delete(e.subs, sid)
			close(ch)
		}
	}
}

// ErrReplayGap means the requested cursor is older than the ring buffer holds.
var ErrReplayGap = errors.New("requested seq is before buffer start: use replayFrom:start")

// Replay returns buffered events with seq > afterSeq.
func (r *Registry) Replay(id string, afterSeq uint64) ([]backend.Event, error) {
	e := r.entry(id)
	if e == nil {
		return nil, ErrSessionNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.replayLocked(afterSeq)
}

// SubscribeReplay returns the buffered events after afterSeq and subscribes to
// later ones as one step, so no event is lost or delivered twice between the
// replay and the live stream. If afterSeq is nil there is no replay. It also
// returns the session's current foreground state.
func (r *Registry) SubscribeReplay(id string, afterSeq *uint64) ([]backend.Event, backend.TurnEvent, <-chan backend.Event, func(), error) {
	e := r.entry(id)
	if e == nil {
		return nil, backend.TurnEvent{}, nil, nil, ErrSessionNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var replay []backend.Event
	if afterSeq != nil {
		var err error
		if replay, err = e.replayLocked(*afterSeq); err != nil {
			return nil, backend.TurnEvent{}, nil, nil, err
		}
	}
	ch, unsub := e.subscribeLocked()
	return replay, e.state, ch, unsub, nil
}

func (r *Registry) entry(id string) *entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[id]
}

func (e *entry) replayLocked(afterSeq uint64) ([]backend.Event, error) {
	if e.bufSeq == 0 {
		return nil, nil
	}
	var out []backend.Event
	if e.bufHead <= e.bufCap {
		// The buffer has not wrapped: only slots [0, bufHead) are valid.
		if afterSeq < e.buf[0].seq && e.bufHead > 0 {
			return nil, ErrReplayGap
		}
		for i := 0; i < e.bufHead; i++ {
			if s := e.buf[i]; s.seq > afterSeq {
				out = append(out, s.ev)
			}
		}
		return out, nil
	}
	// The buffer has wrapped: the oldest entry is at bufHead % bufCap.
	oldest := e.bufHead % e.bufCap
	if afterSeq < e.buf[oldest].seq {
		return nil, ErrReplayGap
	}
	for i := 0; i < e.bufCap; i++ {
		if s := e.buf[(oldest+i)%e.bufCap]; s.seq > afterSeq {
			out = append(out, s.ev)
		}
	}
	return out, nil
}

// Get returns the handle for id.
func (r *Registry) Get(id string) (backend.SessionHandle, error) {
	r.mu.RLock()
	e := r.sessions[id]
	r.mu.RUnlock()
	if e == nil {
		return nil, ErrSessionNotFound
	}
	return e.handle, nil
}

// Remove closes and removes a session.
func (r *Registry) Remove(ctx context.Context, id string) error {
	r.mu.Lock()
	e := r.sessions[id]
	delete(r.sessions, id)
	r.mu.Unlock()
	if e == nil {
		return nil
	}
	return e.handle.Close(ctx)
}

// IDs returns all registered session IDs.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		out = append(out, id)
	}
	return out
}
