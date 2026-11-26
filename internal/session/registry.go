package session

import (
	"context"
	"errors"
	"sync"

	"github.com/coxlong/harness-acp/internal/backend"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// ErrSessionNotFound is returned when a session ID is unknown.
var ErrSessionNotFound = errors.New("session not found")

// ErrTurnConflict is returned when a concurrent turn is already running.
var ErrTurnConflict = errors.New("turn already in progress")

// entry holds live state for one active session.
type entry struct {
	mu        sync.Mutex
	handle    backend.SessionHandle
	agentName string

	subs   map[uint64]*subscriber
	subSeq uint64
	closed bool
	state  backend.TurnEvent
	usage  schema.SessionUpdateUsageUpdate
	output *liveOutput
}

type subscriber struct {
	events        chan backend.Event
	overflow      chan struct{}
	canElicitForm bool
	ctx           context.Context
	cancel        context.CancelFunc
}

type Subscription struct {
	Events      <-chan backend.Event
	Overflow    <-chan struct{}
	LiveOutput  []backend.HistoryUpdate
	State       backend.TurnEvent
	Usage       schema.SessionUpdateUsageUpdate
	Unsubscribe func()
}

// Registry holds all active sessions for one backend endpoint.
type Registry struct {
	mu        sync.RWMutex
	sessions  map[string]*entry
	lifecycle map[string]*lifecycleLock
}

type lifecycleLock struct {
	token chan struct{}
	users int
}

// New creates a session Registry.
func New() *Registry {
	return &Registry{
		sessions:  make(map[string]*entry),
		lifecycle: make(map[string]*lifecycleLock),
	}
}

// LockLifecycle serializes operations that attach or unload a native session,
// including requests from different ACP connections.
func (r *Registry) LockLifecycle(ctx context.Context, id string) (func(), error) {
	r.mu.Lock()
	gate := r.lifecycle[id]
	if gate == nil {
		gate = &lifecycleLock{token: make(chan struct{}, 1)}
		r.lifecycle[id] = gate
	}
	gate.users++
	r.mu.Unlock()

	releaseRef := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		gate.users--
		if gate.users == 0 {
			delete(r.lifecycle, id)
		}
	}
	select {
	case gate.token <- struct{}{}:
		return func() {
			<-gate.token
			releaseRef()
		}, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

// Register adds a new session handle to the registry.
func (r *Registry) Register(id, agentName string, h backend.SessionHandle) {
	e := &entry{
		handle: h, agentName: agentName,
		subs: make(map[uint64]*subscriber),
	}
	if agentName == "codex" {
		e.output = newLiveOutput()
	}
	r.mu.Lock()
	if previous := r.sessions[id]; previous != nil && previous.handle == h {
		r.mu.Unlock()
		return
	}
	r.sessions[id] = e
	r.mu.Unlock()
	go r.pump(id, e)
}

// pump fans out backend events to live subscribers and maintains the current
// state/usage snapshot for cold resumes. User interactions go to at most one
// subscriber (the most recently attached capable connection).
func (r *Registry) pump(id string, e *entry) {
	for ev := range e.handle.Events() {
		e.mu.Lock()
		if ev.Kind == backend.EventTurnState && ev.Session == "" {
			e.state = backend.TurnEvent{State: ev.Turn.State, Background: ev.Turn.Background}
		}
		delivered := false
		switch ev.Kind {
		case backend.EventElicitation, backend.EventPermission:
			var latest uint64
			var selected *subscriber
			for key, sub := range e.subs {
				if ev.Kind == backend.EventElicitation && !sub.canElicitForm {
					continue
				}
				if selected == nil || key > latest {
					latest, selected = key, sub
				}
			}
			if selected != nil {
				delivered = e.send(latest, selected, ev)
				if delivered && ev.Kind == backend.EventElicitation {
					request := ev.Elicitation
					stop := context.AfterFunc(selected.ctx, func() { request.Reply(nil) })
					context.AfterFunc(request.Context, func() { stop() })
				}
			}
		default:
			if ev.Kind == backend.EventUpdate && ev.Session == "" {
				if usage, ok := ev.Update.Variant().(schema.SessionUpdateUsageUpdate); ok && usage.Size > 0 {
					if usage.Cost.IsZero() {
						usage.Cost = e.usage.Cost
					}
					e.usage = usage
				}
			}
			e.output.observe(ev)
			for key, sub := range e.subs {
				e.send(key, sub, ev)
			}
		}
		e.mu.Unlock()
		if !delivered {
			switch ev.Kind {
			case backend.EventPermission:
				_ = e.handle.Respond(ev.Permission.ReqID, backend.Decision{})
			case backend.EventElicitation:
				ev.Elicitation.Reply(nil)
			}
		}
	}

	r.mu.Lock()
	if r.sessions[id] == e {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	e.mu.Lock()
	e.closed = true
	for key, sub := range e.subs {
		sub.cancel()
		close(sub.events)
		delete(e.subs, key)
	}
	e.mu.Unlock()
}

func (e *entry) send(id uint64, sub *subscriber, ev backend.Event) bool {
	select {
	case sub.events <- ev:
		return true
	default:
		delete(e.subs, id)
		close(sub.overflow)
		sub.cancel()
		close(sub.events)
		return false
	}
}

// Attach subscribes to subsequent events and captures the current state, usage,
// and (for fromStart) in-flight output snapshot atomically under one lock.
func (r *Registry) Attach(id string, canElicitForm, fromStart bool) (Subscription, error) {
	r.mu.RLock()
	e := r.sessions[id]
	r.mu.RUnlock()
	if e == nil {
		return Subscription{}, ErrSessionNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Subscription{}, ErrSessionNotFound
	}
	var output []backend.HistoryUpdate
	if fromStart {
		output = e.output.snapshot()
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub := &subscriber{
		events: make(chan backend.Event, 256), overflow: make(chan struct{}),
		canElicitForm: canElicitForm, ctx: ctx, cancel: cancel,
	}
	key := e.subSeq
	e.subSeq++
	e.subs[key] = sub
	return Subscription{
		Events: sub.events, Overflow: sub.overflow, LiveOutput: output, State: e.state, Usage: e.usage,
		Unsubscribe: func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if _, ok := e.subs[key]; ok {
				delete(e.subs, key)
				sub.cancel()
				close(sub.events)
			}
		},
	}, nil
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
