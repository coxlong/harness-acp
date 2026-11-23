package acp

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/acp-go"

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
	attached map[string]*subscription
}

type subscription struct {
	session.Subscription
	started chan struct{}
	done    chan struct{}
}

// NewFacade creates a Facade. conn is provided by NewAgentSideConnection's
// newAgent callback, so it is non-nil whenever a method runs.
func NewFacade(name string, be backend.Backend, reg *session.Registry, allowDirs []string, conn *acp2.AgentSideConnection) *Facade {
	return &Facade{
		name: name, be: be, reg: reg, allowDirs: allowDirs, conn: conn,
		attached: make(map[string]*subscription),
	}
}

// backendError turns a backend error into the ACP error the protocol assigns
// it: typed client-input problems keep their class (-32602 invalid params,
// -32002 unknown session with data.sessionId), and anything else is wrapped
// with the operation's name and reported as an internal error.
func backendError(err error, op string) error {
	var ip *backend.InvalidParamsError
	if errors.As(err, &ip) {
		return acp.InvalidParams(ip.Detail)
	}
	var gone *backend.CwdNotExistError
	if errors.As(err, &gone) {
		return acp.InvalidParams(gone.Error())
	}
	var missing *backend.SessionNotFoundError
	if errors.As(err, &missing) {
		return acp.SessionNotFound(missing.ID)
	}
	if op != "" {
		return fmt.Errorf("%s: %w", op, err)
	}
	return err
}

// Initialize negotiates the ACP v2 protocol version.
func (f *Facade) Initialize(_ context.Context, _ *acp2.InitializeRequest) (*acp2.InitializeResponse, error) {
	caps := acp2.CapabilitiesOf(f)
	if f.name == "codex" {
		caps.Session.Prompt = &schema.PromptCapabilities{Image: &schema.PromptImageCapabilities{}}
		caps.Session.AdditionalDirectories = &schema.SessionAdditionalDirectoriesCapabilities{}
	}
	if _, ok := f.be.(backend.SessionManager); !ok {
		caps.Session.Fork, caps.Session.Archive, caps.Session.Title = nil, nil, nil
	}
	return &acp2.InitializeResponse{
		ProtocolVersion: acp2.ProtocolVersion,
		Capabilities:    caps,
	}, nil
}

// NewSession creates a session and subscribes this connection to it.
func (f *Facade) NewSession(ctx context.Context, params *acp2.NewSessionRequest) (*acp2.NewSessionResponse, error) {
	cwd := string(params.Cwd)
	if !cwdAllowed(cwd, f.allowDirs) {
		return nil, acp.InvalidParams(fmt.Sprintf("cwd %q is not under an allowed root", cwd))
	}
	var addDirs []string
	for _, d := range params.AdditionalDirectories {
		addDirs = append(addDirs, string(d))
	}

	initial, err := initialConfig(params.ConfigOptions)
	if err != nil {
		return nil, err
	}
	handle, err := f.be.NewSession(ctx, backend.NewSessionParams{Cwd: cwd, AdditionalDirectories: addDirs, Config: initial})
	if err != nil {
		log.Printf("new session rejected: cwd=%s: %v", cwd, err)
		return nil, backendError(err, "new session")
	}
	id := handle.NativeID()
	f.reg.Register(id, f.name, handle)
	_, start, err := f.attach(id, nil, false)
	if err != nil {
		return nil, err
	}
	start() // a new session has nothing to replay
	opts := handle.Options()
	return &acp2.NewSessionResponse{
		SessionID:         acp2.SessionID(id),
		ConfigOptions:     opts.Config,
		AvailableCommands: opts.Commands,
	}, nil
}

// GetSessionOptions reports the config options and commands a session has, or
// would start with in cwd, without creating or changing a session.
func (f *Facade) GetSessionOptions(ctx context.Context, params *acp2.SessionOptionsRequest) (*acp2.SessionOptionsResponse, error) {
	if params.SessionID != nil {
		if handle, err := f.reg.Get(string(*params.SessionID)); err == nil {
			o := handle.Options()
			return &acp2.SessionOptionsResponse{ConfigOptions: o.Config, AvailableCommands: o.Commands}, nil
		}
		// Not active here: a session of that directory offers the same.
	}
	cwd := string(params.Cwd)
	if !cwdAllowed(cwd, f.allowDirs) {
		return nil, acp.InvalidParams(fmt.Sprintf("cwd %q is not under an allowed root", cwd))
	}
	var dirs []string
	for _, d := range params.AdditionalDirectories {
		dirs = append(dirs, string(d))
	}
	o, err := f.be.Options(ctx, cwd, dirs)
	if err != nil {
		return nil, backendError(err, "session options")
	}
	return &acp2.SessionOptionsResponse{ConfigOptions: o.Config, AvailableCommands: o.Commands}, nil
}

// SetSessionConfigOption changes one option of a session. The response lists
// all options; other connections watching the session get a config_option_update.
func (f *Facade) SetSessionConfigOption(ctx context.Context, params *acp2.SetSessionConfigOptionRequest) (*acp2.SetSessionConfigOptionResponse, error) {
	change, ok := acp2.ConfigChangeOf(params)
	if !ok || change.Custom != nil {
		return nil, acp.InvalidParams("unsupported config option request")
	}
	handle, err := f.reg.Get(string(change.SessionID))
	if err != nil {
		return nil, acp.SessionNotFound(string(change.SessionID))
	}
	var value any = string(change.Value)
	if change.Boolean != nil {
		value = *change.Boolean
	}
	opts, err := handle.SetConfigOption(ctx, string(change.ConfigID), value)
	if err != nil {
		return nil, backendError(err, "")
	}
	return &acp2.SetSessionConfigOptionResponse{ConfigOptions: opts}, nil
}

// sessionListPage is the page size of session/list, as claude-agent-acp's.
const sessionListPage = 1000

// ListSessions lists the backend's sessions, newest first, a page at a time.
// The cursor is `offset:N`, as claude-agent-acp words it; one that is not is
// invalid params.
func (f *Facade) ListSessions(ctx context.Context, params *acp2.ListSessionsRequest) (*acp2.ListSessionsResponse, error) {
	filter := backend.ListFilter{Limit: sessionListPage}
	if params.Archived != nil {
		if _, ok := f.be.(backend.SessionManager); !ok || !params.Archived.Known() {
			return nil, acp.InvalidParams("unsupported archive filter")
		}
		filter.Archived = string(*params.Archived)
	}
	if params.Cwd != nil && *params.Cwd != "" {
		cwd := string(*params.Cwd)
		if !filepath.IsAbs(cwd) {
			return nil, acp.InvalidParams(fmt.Sprintf("cwd %q must be an absolute path", cwd))
		}
		filter.Cwd = cwd
	}
	var sessions []backend.SessionInfo
	var nextCursor *schema.SessionListCursor
	var err error
	if native, ok := f.be.(backend.CursorLister); ok {
		var cursor *string
		if params.Cursor != nil {
			cursor = new(string(*params.Cursor))
		}
		var next *string
		sessions, next, err = native.ListSessionsPage(ctx, filter, cursor)
		if next != nil {
			nextCursor = new(schema.SessionListCursor(*next))
		}
	} else {
		if params.Cursor != nil {
			offset, ok := parseOffsetCursor(string(*params.Cursor))
			if !ok {
				return nil, acp.InvalidParams(fmt.Sprintf("unknown session/list cursor: %s", *params.Cursor))
			}
			filter.Offset = offset
		}
		var more bool
		sessions, more, err = f.be.ListSessions(ctx, filter)
		if more {
			nextCursor = new(schema.SessionListCursor(fmt.Sprintf("offset:%d", filter.Offset+sessionListPage)))
		}
	}
	if err != nil {
		return nil, backendError(err, "list sessions")
	}
	infos := make([]acp2.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		title := s.Title
		updated := s.UpdatedAt.UTC().Format(time.RFC3339Nano)
		infos = append(infos, acp2.SessionInfo{
			SessionID: acp2.SessionID(s.ID),
			Cwd:       schema.AbsolutePath(s.Cwd),
			Title:     &title,
			UpdatedAt: &updated,
			Archived:  s.Archived,
		})
	}
	return &acp2.ListSessionsResponse{Sessions: infos, NextCursor: nextCursor}, nil
}

// parseOffsetCursor reads an `offset:N` cursor.
func parseOffsetCursor(c string) (int, bool) {
	rest, ok := strings.CutPrefix(c, "offset:")
	if !ok || rest == "" {
		return 0, false
	}
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' || n > 1<<40 {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
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
	initial, err := initialConfig(params.ConfigOptions)
	if err != nil {
		return nil, err
	}
	unlock, err := f.reg.LockLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	active, err := f.reg.Get(id)
	if err != nil {
		cwd := string(params.Cwd)
		if cwd != "" && !cwdAllowed(cwd, f.allowDirs) {
			return nil, acp.InvalidParams(fmt.Sprintf("cwd %q is not under an allowed root", cwd))
		}
		var dirs []string
		for _, dir := range params.AdditionalDirectories {
			dirs = append(dirs, string(dir))
		}
		setup := initial
		if _, start := params.ReplayFrom.Variant().(schema.ReplayFromStart); !start && !params.ReplayFrom.IsZero() {
			setup = nil
		}
		handle, err := f.be.ResumeSession(ctx, id, backend.ResumeParams{Cwd: cwd, AdditionalDirectories: dirs, Config: setup})
		if err != nil {
			log.Printf("session %s: resume rejected: cwd=%s: %v", id, cwd, err)
			return nil, backendError(err, "resume session")
		}
		f.reg.Register(id, f.name, handle)
	}

	var cursor *session.Cursor
	fromStart := false
	if !params.ReplayFrom.IsZero() {
		switch v := params.ReplayFrom.Variant().(type) {
		case schema.ReplayFromStart:
			fromStart = true
		case schema.ReplayFromCustom:
			value, ok := parseHarnessCursor(v)
			if !ok {
				return nil, acp.InvalidParams(fmt.Sprintf("unrecognised replayFrom cursor %q", v.Type))
			}
			cursor = &value
		default:
			return nil, acp.InvalidParams("unsupported replay cursor")
		}
	}

	sub, start, err := f.attach(id, cursor, fromStart)
	if err != nil {
		if errors.Is(err, session.ErrReplayGap) {
			return nil, acp.InvalidParams(err.Error())
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
	if setter, ok := active.(interface {
		ConfigureInitial(context.Context, []backend.ConfigSelection) error
	}); ok && len(initial) > 0 {
		if err := setter.ConfigureInitial(ctx, initial); err != nil {
			return nil, backendError(err, "resume configuration")
		}
	}

	if fromStart || cursor != nil {
		sid := params.SessionID
		var hist backend.History
		if fromStart {
			var err error
			hist, err = f.be.History(ctx, id)
			if err != nil {
				return nil, backendError(err, "replay history")
			}
			children := make(map[string]bool, len(hist.Children))
			for _, child := range hist.Children {
				children[child.ID] = true
			}
			for _, u := range sub.LiveOutput {
				if u.Session == "" || children[u.Session] {
					hist.Updates = append(hist.Updates, u)
				}
			}
			for _, u := range hist.Updates {
				target := sid
				if u.Session != "" {
					target = acp2.SessionID(u.Session)
				}
				if err := f.sendUpdate(ctx, target, u.Update); err != nil {
					return nil, err
				}
			}
			// Every child's end goes out on its parent before the replay ends, then
			// each child's own replay is complete: deeper children first.
			for i := len(hist.Children) - 1; i >= 0; i-- {
				c := hist.Children[i]
				if err := f.subagentState(ctx, sid, c, false); err != nil {
					return nil, err
				}
			}
			for _, c := range hist.Children {
				if err := acp2.NewSessionStream(f.conn, acp2.SessionID(c.ID)).ReplayComplete(ctx); err != nil {
					return nil, err
				}
			}
		}
		for _, ev := range sub.Replay {
			if err := f.emit(ctx, sid, ev); err != nil {
				return nil, err
			}
		}
		// replay_complete once per request with replayFrom, empty history too.
		if err := acp2.NewSessionStream(f.conn, sid).ReplayComplete(ctx); err != nil {
			return nil, err
		}
		if hist.Missing > 0 {
			desc := fmt.Sprintf("The history of %d subagents of this session could not be restored.", hist.Missing)
			if hist.Missing == 1 {
				desc = "The history of 1 subagent of this session could not be restored."
			}
			if err := acp2.NewSessionStream(f.conn, sid).Send(ctx, schema.SessionUpdateNotice{
				Severity: schema.NoticeSeverityInfo, Title: "Some subagent history is not available", Description: &desc,
			}); err != nil {
				return nil, err
			}
		}
		// What is current of the replayed children: the end of each one that does
		// not work in the live session, on its parent and on its own stream. The
		// ends above are history; these follow the parents' replay_complete.
		handle, _ := f.reg.Get(id)
		live, _ := handle.(childWorker)
		for _, c := range hist.Children {
			if live != nil && live.ChildWorking(c.ID) {
				continue
			}
			if err := f.subagentState(ctx, sid, c, true); err != nil {
				return nil, err
			}
		}
	}

	// The snapshot tells the client whether a turn is open right now.
	if err := f.snapshot(ctx, params.SessionID, sub.State, sub.Cursor); err != nil {
		return nil, err
	}
	resumed = true
	handle, _ := f.reg.Get(id)
	var opts backend.Options
	if handle != nil {
		opts = handle.Options()
	}
	return &acp2.ResumeSessionResponse{ConfigOptions: opts.Config, AvailableCommands: opts.Commands}, nil
}

// CloseSession closes a session and drops this connection's subscription.
func (f *Facade) CloseSession(ctx context.Context, params *acp2.CloseSessionRequest) (*acp2.CloseSessionResponse, error) {
	id := string(params.SessionID)
	unlock, err := f.reg.LockLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := f.closeSession(ctx, id); err != nil {
		return nil, err
	}
	return &acp2.CloseSessionResponse{}, nil
}

func (f *Facade) closeSession(ctx context.Context, id string) error {
	if f.name != "codex" {
		f.detach(id)
		_ = f.reg.Remove(ctx, id)
		return nil
	}

	f.mu.Lock()
	sub := f.attached[id]
	f.mu.Unlock()
	err := f.reg.Remove(ctx, id)
	if sub != nil {
		select {
		case <-sub.started:
			// Once the native session closes, the forwarder drains its final
			// cancelled update before this connection receives the close reply.
			select {
			case <-sub.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			f.detach(id)
		}
	}
	if err != nil {
		return backendError(err, "close session")
	}
	return nil
}

// DeleteSession removes a session and its stored history. An active session is
// stopped first, so nothing keeps writing what is being deleted; a session
// that is not there is not an error, as the protocol asks.
func (f *Facade) DeleteSession(ctx context.Context, params *acp2.DeleteSessionRequest) (*acp2.DeleteSessionResponse, error) {
	id := string(params.SessionID)
	unlock, err := f.reg.LockLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	f.detach(id)
	if err := f.reg.Remove(ctx, id); err != nil {
		return nil, backendError(err, "delete session "+id)
	}
	if err := f.be.DeleteSession(ctx, id); err != nil {
		log.Printf("session %s: delete rejected: %v", id, err)
		return nil, backendError(err, "delete session "+id)
	}
	return &acp2.DeleteSessionResponse{}, nil
}

// CancelSession interrupts the running turn. The turn's end is still reported
// by the session's own idle(cancelled), not here.
func (f *Facade) CancelSession(ctx context.Context, params *acp2.CancelSessionNotification) error {
	handle, err := f.reg.Get(string(params.SessionID))
	if err != nil {
		// Not a session of its own: it may be a subagent's child session, which
		// its parent stops.
		return f.cancelChild(ctx, string(params.SessionID))
	}
	return handle.Cancel(ctx)
}

// childWorker is a session that knows which of its subagents work right now.
type childWorker interface {
	ChildWorking(id string) bool
}

// subagentState reports how a replayed child's work ended, on its parent's
// stream; with own, on the child's own stream too.
func (f *Facade) subagentState(ctx context.Context, root acp2.SessionID, c backend.ReplayedChild, own bool) error {
	parent := root
	if c.Parent != "" {
		parent = acp2.SessionID(c.Parent)
	}
	st := stateUpdate(c.End)
	if err := acp2.NewSessionStream(f.conn, parent).Send(ctx, schema.SessionUpdateSubagentUpdate{
		SessionID: schema.SessionID(c.ID), State: optional.Of(st),
	}); err != nil {
		return err
	}
	if !own {
		return nil
	}
	return acp2.NewSessionStream(f.conn, acp2.SessionID(c.ID)).Send(ctx, schema.SessionUpdateStateUpdate{Value: st})
}

// childCanceller is a session that has child sessions (subagents) to stop.
type childCanceller interface {
	CancelChild(ctx context.Context, id string) (bool, error)
}

// terminalCanceller is a session that can stop the background commands of its
// terminals, and of its subagents' terminals.
type terminalCanceller interface {
	OwnsSession(id string) bool
	CancelTerminal(ctx context.Context, sessionID, terminal string) (bool, error)
}

// CancelTerminal stops a background command, as terminal/cancel asks. The
// terminal's exit confirms it. A terminal that is not a running background
// command is refused.
func (f *Facade) CancelTerminal(ctx context.Context, params *acp2.CancelTerminalRequest) (*acp2.CancelTerminalResponse, error) {
	sid, tid := string(params.SessionID), string(params.TerminalID)
	owned := false
	for _, id := range f.reg.IDs() {
		h, err := f.reg.Get(id)
		if err != nil {
			continue
		}
		c, ok := h.(terminalCanceller)
		if !ok || !c.OwnsSession(sid) {
			continue
		}
		owned = true
		found, err := c.CancelTerminal(ctx, sid, tid)
		if err != nil {
			return nil, err
		}
		if found {
			return &acp2.CancelTerminalResponse{}, nil
		}
	}
	if !owned {
		return nil, acp.SessionNotFound(sid)
	}
	return nil, acp.InvalidParams(fmt.Sprintf("terminal %s is not a command running in the background, so it cannot be cancelled", tid))
}

// toolCallCanceller is a session that can stop the background tasks that its
// tool calls stand for, and its subagents' too.
type toolCallCanceller interface {
	OwnsSession(id string) bool
	CancelToolCall(ctx context.Context, sessionID, toolCall string) (bool, error)
}

// CancelToolCall stops a background task, as tool_call/cancel asks: a Monitor
// or Workflow call whose task goes on, or the call that stands for a task that
// nothing else shows. The call's final status confirms it. A tool call that is
// not listed as background work is refused.
func (f *Facade) CancelToolCall(ctx context.Context, params *acp2.CancelToolCallRequest) (*acp2.CancelToolCallResponse, error) {
	sid, cid := string(params.SessionID), string(params.ToolCallID)
	owned := false
	for _, id := range f.reg.IDs() {
		h, err := f.reg.Get(id)
		if err != nil {
			continue
		}
		c, ok := h.(toolCallCanceller)
		if !ok || !c.OwnsSession(sid) {
			continue
		}
		owned = true
		found, err := c.CancelToolCall(ctx, sid, cid)
		if err != nil {
			return nil, err
		}
		if found {
			return &acp2.CancelToolCallResponse{}, nil
		}
	}
	if !owned {
		return nil, acp.SessionNotFound(sid)
	}
	return nil, acp.InvalidParams(fmt.Sprintf("tool call %s is not running in the background, so it cannot be cancelled", cid))
}

// cancelChild stops the subagent whose child session is id, in whichever
// session owns it. An id nobody owns is ignored, as a cancel of an inactive session is.
func (f *Facade) cancelChild(ctx context.Context, id string) error {
	for _, sid := range f.reg.IDs() {
		h, err := f.reg.Get(sid)
		if err != nil {
			continue
		}
		if c, ok := h.(childCanceller); ok {
			if found, err := c.CancelChild(ctx, id); found {
				return err
			}
		}
	}
	return nil
}

// Prompt accepts a message and returns at once (v2): the turn runs on, and its
// end is reported by state_update idle. The session must be attached on this
// connection, or its output would have nowhere to go.
func (f *Facade) Prompt(ctx context.Context, params *acp2.PromptRequest) (*acp2.PromptResponse, error) {
	id := string(params.SessionID)
	handle, err := f.reg.Get(id)
	if err != nil {
		return nil, acp.SessionNotFound(id)
	}
	f.mu.Lock()
	_, attached := f.attached[id]
	f.mu.Unlock()
	if !attached {
		return nil, acp.ResourceNotFound(
			fmt.Sprintf("session %s is not active on this connection: send session/resume first", id),
		).WithData(map[string]string{"sessionId": id})
	}
	msgID, err := handle.Send(ctx, params.Prompt)
	if err != nil {
		return nil, backendError(err, "")
	}
	return &acp2.PromptResponse{MessageID: msgID}, nil
}

// --- subscription -----------------------------------------------------------

// attach subscribes this connection to the session and returns what to send
// before live events: the ring-buffer replay (when afterSeq is set), the
// session's current state, and a start func that begins forwarding live
// events. Attaching twice replaces the earlier subscription.
func (f *Facade) attach(id string, cursor *session.Cursor, fromStart bool) (*subscription, func(), error) {
	caps := f.conn.ClientCapabilities()
	form := caps != nil && caps.Elicitation != nil && caps.Elicitation.Form != nil
	attached, err := f.reg.Attach(id, cursor, form, fromStart)
	if err != nil {
		return nil, nil, err
	}
	sub := &subscription{Subscription: attached, started: make(chan struct{}), done: make(chan struct{})}
	f.mu.Lock()
	if old := f.attached[id]; old != nil {
		old.Unsubscribe()
	}
	f.attached[id] = sub
	f.mu.Unlock()

	start := sync.OnceFunc(func() {
		close(sub.started)
		go f.forward(acp2.SessionID(id), sub)
	})
	return sub, start, nil
}

func (f *Facade) detach(id string) {
	f.mu.Lock()
	sub := f.attached[id]
	delete(f.attached, id)
	f.mu.Unlock()
	if sub != nil {
		sub.Unsubscribe()
	}
}

// forward sends a session's live events to this connection until the
// subscription closes or the connection ends. One goroutine per attached
// session.
func (f *Facade) forward(id acp2.SessionID, sub *subscription) {
	defer close(sub.done)
	defer func() {
		sub.Unsubscribe()
		f.mu.Lock()
		if f.attached[string(id)] == sub {
			delete(f.attached, string(id))
		}
		f.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-f.conn.Done():
			cancel() // also aborts a permission request still waiting for the user
		case <-sub.Overflow:
			cancel()
			_ = f.conn.Close()
		case <-ctx.Done():
		}
	}()

	stream := acp2.NewSessionStream(f.conn, id)
	for {
		var ev session.Delivery
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub.Events:
			if !ok {
				select {
				case <-sub.Overflow:
					_ = f.conn.Close()
				default:
				}
				return
			}
			ev = e
		}
		switch ev.Kind {
		case backend.EventPermission:
			go f.askPermission(ctx, stream, id, ev.Permission)
			continue
		case backend.EventElicitation:
			go f.elicit(ctx, ev.Elicitation)
			continue
		}
		if err := f.emit(ctx, id, ev); err != nil {
			log.Printf("session %s: forwarding failed: %v", id, err)
			return
		}
	}
}

// emit sends one non-permission event, to the session it belongs to: a child
// session's events go to the child.
func (f *Facade) emit(ctx context.Context, id acp2.SessionID, ev session.Delivery) error {
	if ev.Session != "" {
		id = acp2.SessionID(ev.Session)
	}
	var update acp2.SessionUpdate
	switch ev.Kind {
	case backend.EventUpdate:
		update = ev.Update
	case backend.EventTurnState:
		update = schema.NewSessionUpdate(schema.SessionUpdateStateUpdate{Value: stateUpdate(ev.Turn)})
	case backend.EventFatal:
		log.Printf("session %s fatal: %v", id, ev.Err)
		update = schema.NewSessionUpdate(schema.SessionUpdateStateUpdate{Value: stateUpdate(backend.TurnEvent{Err: ev.Err})})
	default:
		return nil
	}
	return f.conn.SessionUpdate(ctx, &acp2.UpdateSessionNotification{
		SessionID: id, Update: update, Meta: cursorMeta(ev.Cursor),
	})
}

func (f *Facade) elicit(ctx context.Context, request *backend.Elicitation) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(request.Context, cancel)
	defer stop()
	if request.Context.Err() != nil {
		return
	}
	response, err := f.conn.CreateElicitation(ctx, &request.Request)
	if err != nil {
		response = nil
	}
	request.Reply(response)
}

// stateUpdate builds the state_update for a turn event, with its background
// work. acp2's SessionStream helpers cannot carry that.
func stateUpdate(t backend.TurnEvent) schema.StateUpdate {
	switch t.State {
	case backend.TurnRunning:
		return schema.NewStateUpdate(schema.StateUpdateRunning{Background: t.Background})
	case backend.TurnRequiresAction:
		return schema.NewStateUpdate(schema.StateUpdateRequiresAction{Background: t.Background})
	}
	return schema.NewStateUpdate(schema.StateUpdateIdle{Value: idleUpdate(t)})
}

func idleUpdate(t backend.TurnEvent) schema.IdleStateUpdate {
	bg := t.Background
	if t.Err != nil {
		re, ok := errors.AsType[*acp.RequestError](t.Err)
		if !ok {
			re = acp.InternalError(t.Err.Error())
		}
		e := &schema.Error{Code: schema.ErrorCode(re.Code), Message: re.Message}
		if re.Data != nil {
			if raw, err := json.Marshal(re.Data); err == nil {
				e.Data, _ = schema.NewErrorData(jsontext.Value(raw))
			}
		}
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateError{Error: e, Background: bg})
	}
	switch t.StopReason {
	case acp2.StopReasonEndTurn:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateEndTurn{Background: bg})
	case acp2.StopReasonMaxTokens:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateMaxTokens{Background: bg})
	case acp2.StopReasonMaxTurnRequests:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateMaxTurnRequests{Background: bg})
	case acp2.StopReasonRefusal:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateRefusal{Background: bg})
	case acp2.StopReasonCancelled:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateCancelled{Background: bg})
	}
	// No stop reason: a snapshot of an idle session, not a completion.
	return schema.NewIdleStateUpdate(schema.IdleStateUpdateUntagged{Background: bg})
}

// snapshot reports the foreground state a resuming client joins in the middle
// of. An idle snapshot carries no stop reason: it is not a new completion.
func (f *Facade) snapshot(ctx context.Context, id acp2.SessionID, state backend.TurnEvent, cursor session.Cursor) error {
	state.StopReason, state.Err = "", nil
	return f.emit(ctx, id, session.Delivery{
		Event: backend.Event{Kind: backend.EventTurnState, Turn: state}, Cursor: cursor,
	})
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
	// A subagent's request is asked in its own session.
	if p.Session != "" {
		stream = acp2.NewSessionStream(f.conn, acp2.SessionID(p.Session))
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

const replayCursorType = "_harness/seq"

func cursorMeta(cursor session.Cursor) schema.Meta {
	var meta schema.Meta
	_ = meta.Set("_harness/replayCursor", struct {
		Type string `json:"type"`
		session.Cursor
	}{replayCursorType, cursor})
	return meta
}

func parseHarnessCursor(c schema.ReplayFromCustom) (session.Cursor, bool) {
	var cursor session.Cursor
	if c.Type != replayCursorType {
		return cursor, false
	}
	if json.Unmarshal(c.AdditionalProperties["generation"], &cursor.Generation) != nil || cursor.Generation == "" {
		return cursor, false
	}
	seq := c.AdditionalProperties["seq"]
	if len(seq) == 0 || string(seq) == "null" || json.Unmarshal(seq, &cursor.Seq) != nil {
		return cursor, false
	}
	return cursor, true
}
