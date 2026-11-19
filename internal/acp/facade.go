package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Facade implements acp2.Agent for one backend on one connection.
//
// It holds no session state: sessions live in the session.Registry and outlive
// the connection. The only per-connection state is which sessions this
// connection is subscribed to, so that each gets exactly one forwarding
// goroutine however many prompts it receives.
type Facade struct {
	name      string
	be        backend.Backend
	reg       *session.Registry
	allowDirs []string
	conn      *acp2.AgentSideConnection

	mu       sync.Mutex
	attached map[string]func() // session id → unsubscribe
}

// NewFacade creates a Facade. conn is provided by NewAgentSideConnection's
// newAgent callback, so it is non-nil whenever a method runs.
func NewFacade(name string, be backend.Backend, reg *session.Registry, allowDirs []string, conn *acp2.AgentSideConnection) *Facade {
	return &Facade{
		name: name, be: be, reg: reg, allowDirs: allowDirs, conn: conn,
		attached: make(map[string]func()),
	}
}

// Initialize negotiates the ACP v2 protocol version.
func (f *Facade) Initialize(_ context.Context, _ *acp2.InitializeRequest) (*acp2.InitializeResponse, error) {
	return &acp2.InitializeResponse{
		ProtocolVersion: acp2.ProtocolVersion,
		Capabilities:    acp2.CapabilitiesOf(f),
	}, nil
}

// NewSession creates a session and subscribes this connection to it.
func (f *Facade) NewSession(ctx context.Context, params *acp2.NewSessionRequest) (*acp2.NewSessionResponse, error) {
	cwd := string(params.Cwd)
	if !cwdAllowed(cwd, f.allowDirs) {
		return nil, fmt.Errorf("invalid params: cwd %q is not under an allowed root", cwd)
	}
	var addDirs []string
	for _, d := range params.AdditionalDirectories {
		addDirs = append(addDirs, string(d))
	}

	handle, err := f.be.NewSession(ctx, backend.NewSessionParams{Cwd: cwd, AdditionalDirectories: addDirs})
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	id := handle.NativeID()
	f.reg.Register(id, f.name, handle)
	_, _, start, err := f.attach(id, nil)
	if err != nil {
		return nil, err
	}
	start() // a new session has nothing to replay
	return &acp2.NewSessionResponse{SessionID: acp2.SessionID(id)}, nil
}

// ListSessions lists the backend's sessions.
func (f *Facade) ListSessions(ctx context.Context, _ *acp2.ListSessionsRequest) (*acp2.ListSessionsResponse, error) {
	sessions, _, err := f.be.ListSessions(ctx, backend.ListFilter{})
	if err != nil {
		return nil, err
	}
	infos := make([]acp2.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		title := s.Title
		infos = append(infos, acp2.SessionInfo{
			SessionID: acp2.SessionID(s.ID),
			Cwd:       schema.AbsolutePath(s.Cwd),
			Title:     &title,
		})
	}
	return &acp2.ListSessionsResponse{Sessions: infos}, nil
}

// ResumeSession attaches this connection to a session, replaying history if
// asked: `start` from the backend's own record, `_harness/seq` from the
// in-memory ring buffer of what this process already sent.
//
// Order on the wire: replayed updates, replay_complete, the state snapshot,
// then live updates. The subscription is taken before the replay is read, so
// nothing that happens meanwhile is lost; live events wait in its channel.
func (f *Facade) ResumeSession(ctx context.Context, params *acp2.ResumeSessionRequest) (*acp2.ResumeSessionResponse, error) {
	id := string(params.SessionID)

	if _, err := f.reg.Get(id); err != nil {
		cwd := string(params.Cwd)
		if cwd != "" && !cwdAllowed(cwd, f.allowDirs) {
			return nil, fmt.Errorf("invalid params: cwd %q is not under an allowed root", cwd)
		}
		handle, err := f.be.ResumeSession(ctx, id, backend.ResumeParams{Cwd: cwd})
		if err != nil {
			return nil, fmt.Errorf("resume session: %w", err)
		}
		f.reg.Register(id, f.name, handle)
	}

	var afterSeq *uint64
	fromStart := false
	if !params.ReplayFrom.IsZero() {
		switch v := params.ReplayFrom.Variant().(type) {
		case schema.ReplayFromStart:
			fromStart = true
		case schema.ReplayFromCustom:
			seq, ok := parseHarnessSeq(v)
			if !ok {
				return nil, fmt.Errorf("invalid params: unrecognised replayFrom cursor %q", v.Type)
			}
			afterSeq = &seq
		}
	}

	replay, state, start, err := f.attach(id, afterSeq)
	if err != nil {
		if errors.Is(err, session.ErrReplayGap) {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, err
	}
	// Live events stay queued in the subscription until start() runs. A resume
	// that fails must not leave the connection subscribed to a session the
	// client was told it could not resume.
	resumed := false
	defer func() {
		if resumed {
			start()
		} else {
			f.detach(id)
		}
	}()

	if fromStart || afterSeq != nil {
		sid := params.SessionID
		if fromStart {
			updates, err := f.be.History(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("replay history: %w", err)
			}
			for _, u := range updates {
				if err := f.sendUpdate(ctx, sid, u); err != nil {
					return nil, err
				}
			}
		}
		for _, ev := range replay {
			if err := f.emit(ctx, acp2.NewSessionStream(f.conn, sid), sid, ev); err != nil {
				return nil, err
			}
		}
		// replay_complete once per request with replayFrom, empty history too.
		if err := acp2.NewSessionStream(f.conn, sid).ReplayComplete(ctx); err != nil {
			return nil, err
		}
	}

	// The snapshot tells the client whether a turn is open right now.
	if err := f.snapshot(ctx, params.SessionID, state); err != nil {
		return nil, err
	}
	resumed = true
	return &acp2.ResumeSessionResponse{}, nil
}

// CloseSession closes a session and drops this connection's subscription.
func (f *Facade) CloseSession(ctx context.Context, params *acp2.CloseSessionRequest) (*acp2.CloseSessionResponse, error) {
	id := string(params.SessionID)
	f.detach(id)
	_ = f.reg.Remove(ctx, id)
	return &acp2.CloseSessionResponse{}, nil
}

// CancelSession interrupts the running turn. The turn's end is still reported
// by the session's own idle(cancelled), not here.
func (f *Facade) CancelSession(ctx context.Context, params *acp2.CancelSessionNotification) error {
	handle, err := f.reg.Get(string(params.SessionID))
	if err != nil {
		return nil // cancelling a session that is not active is ignored
	}
	return handle.Cancel(ctx)
}

// Prompt accepts a message and returns at once (v2): the turn runs on, and its
// end is reported by state_update idle. The session must be attached on this
// connection, or its output would have nowhere to go.
func (f *Facade) Prompt(ctx context.Context, params *acp2.PromptRequest) (*acp2.PromptResponse, error) {
	id := string(params.SessionID)
	handle, err := f.reg.Get(id)
	if err != nil {
		return nil, fmt.Errorf("session %s not found", id)
	}
	f.mu.Lock()
	_, attached := f.attached[id]
	f.mu.Unlock()
	if !attached {
		return nil, fmt.Errorf("session %s is not active on this connection: send session/resume first", id)
	}
	msgID, err := handle.Send(ctx, params.Prompt)
	if err != nil {
		return nil, err
	}
	return &acp2.PromptResponse{MessageID: msgID}, nil
}

// --- subscription -----------------------------------------------------------

// attach subscribes this connection to the session and returns what to send
// before live events: the ring-buffer replay (when afterSeq is set), the
// session's current state, and a start func that begins forwarding live
// events. Attaching twice replaces the earlier subscription.
func (f *Facade) attach(id string, afterSeq *uint64) ([]backend.Event, backend.TurnState, func(), error) {
	replay, state, ch, unsub, err := f.reg.SubscribeReplay(id, afterSeq)
	if err != nil {
		return nil, backend.TurnIdle, nil, err
	}
	f.mu.Lock()
	if old := f.attached[id]; old != nil {
		old() // closes the old channel, which ends its forwarding goroutine
	}
	f.attached[id] = unsub
	f.mu.Unlock()

	started := false
	start := func() {
		if started {
			return
		}
		started = true
		go f.forward(acp2.SessionID(id), ch)
	}
	return replay, state, start, nil
}

func (f *Facade) detach(id string) {
	f.mu.Lock()
	unsub := f.attached[id]
	delete(f.attached, id)
	f.mu.Unlock()
	if unsub != nil {
		unsub()
	}
}

// forward sends a session's live events to this connection until the
// subscription closes or the connection ends. One goroutine per attached
// session.
func (f *Facade) forward(id acp2.SessionID, ch <-chan backend.Event) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.conn.Done():
			cancel() // also aborts a permission request still waiting for the user
		case <-ctx.Done():
		}
	}()

	stream := acp2.NewSessionStream(f.conn, id)
	for ev := range ch {
		if ev.Kind == backend.EventPermission {
			// Answered in its own goroutine: the user may take minutes, and the
			// events behind this one must keep flowing meanwhile.
			go f.askPermission(ctx, stream, id, ev.Permission)
			continue
		}
		if err := f.emit(ctx, stream, id, ev); err != nil {
			log.Printf("session %s: forwarding failed: %v", id, err)
			return
		}
	}
}

// emit sends one non-permission event.
func (f *Facade) emit(ctx context.Context, stream *acp2.SessionStream, id acp2.SessionID, ev backend.Event) error {
	switch ev.Kind {
	case backend.EventUpdate:
		return f.sendUpdate(ctx, id, ev.Update)
	case backend.EventTurnState:
		switch ev.Turn.State {
		case backend.TurnRunning:
			return stream.Running(ctx)
		case backend.TurnRequiresAction:
			return stream.RequiresAction(ctx)
		default:
			if ev.Turn.Err != nil {
				return stream.IdleError(ctx, ev.Turn.Err)
			}
			return stream.Idle(ctx, ev.Turn.StopReason)
		}
	case backend.EventFatal:
		log.Printf("session %s fatal: %v", id, ev.Err)
		return stream.IdleError(ctx, ev.Err)
	}
	return nil
}

// snapshot reports the foreground state a resuming client joins in the middle
// of. An idle snapshot carries no stop reason: it is not a new completion.
func (f *Facade) snapshot(ctx context.Context, id acp2.SessionID, state backend.TurnState) error {
	stream := acp2.NewSessionStream(f.conn, id)
	switch state {
	case backend.TurnRunning:
		return stream.Running(ctx)
	case backend.TurnRequiresAction:
		return stream.RequiresAction(ctx)
	}
	return stream.Idle(ctx, "")
}

func (f *Facade) sendUpdate(ctx context.Context, id acp2.SessionID, u acp2.SessionUpdate) error {
	return f.conn.SessionUpdate(ctx, &acp2.UpdateSessionNotification{SessionID: id, Update: u})
}

// askPermission asks the user and relays the answer to the backend. A request
// that cannot be asked (the connection ended) is refused, so the turn goes on
// instead of waiting for an answer that cannot come.
func (f *Facade) askPermission(ctx context.Context, stream *acp2.SessionStream, id acp2.SessionID, p backend.PermissionRequest) {
	handle, err := f.reg.Get(string(id))
	if err != nil {
		return
	}
	subject := acp2.NewRequestPermissionSubject(schema.RequestPermissionSubjectToolCall{
		ToolCall: schema.ToolCallUpdate{
			ToolCallID: acp2.ToolCallID(p.ToolCallID),
			Name:       optional.Of(p.ToolName),
			Title:      optional.Of(p.Title),
		},
	})
	// Only allow-once and reject-once: the backend does not persist a rule, so
	// offering "always allow" would promise something it cannot keep.
	options := []acp2.PermissionOption{
		acp2.NewPermissionOption(schema.PermissionOptionKindAllowOnce, "Allow"),
		acp2.NewPermissionOption(schema.PermissionOptionKindRejectOnce, "Reject"),
	}
	_, allowed, err := stream.RequestPermission(ctx, p.Title, subject, options...)
	if err != nil {
		allowed = false
	}
	if err := handle.Respond(p.ReqID, backend.Decision{Allow: allowed}); err != nil {
		log.Printf("session %s: answering permission %s: %v", id, p.ReqID, err)
	}
}

// --- helpers ----------------------------------------------------------------

// parseHarnessSeq extracts the seq field from a _harness/seq custom cursor.
func parseHarnessSeq(c schema.ReplayFromCustom) (uint64, bool) {
	if c.Type != "_harness/seq" {
		return 0, false
	}
	raw, ok := c.AdditionalProperties["seq"]
	if !ok {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal([]byte(raw), &n); err != nil || n < 0 {
		return 0, false
	}
	return uint64(n), true
}
