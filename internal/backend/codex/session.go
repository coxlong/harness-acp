package codex

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type turn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Items  []item `json:"items"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type Session struct {
	p   *process
	id  string
	out chan backend.Event

	ops sync.Mutex  // serializes turn submissions and session lifecycle operations
	mu  *sync.Mutex // shared with child sessions to order all outbound events

	parent   *Session
	children map[string]*Session // indexed by the root
	label    string
	last     backend.TurnEvent
	running  bool // Codex can announce a child before its turn starts

	closed       bool
	turnID       string
	turnDone     chan struct{}
	submitting   bool
	staged       []message
	model        string
	provider     string
	effort       string
	mode         string
	sendMode     bool
	fresh        bool
	instructions string
	models       []model
	profile      string
	profiles     []profile
	cwd          string
	deltaItems   map[string]bool
	plans        map[string]string
	published    map[string]bool
	pending      map[string]approval
	inputs       map[string]pendingInput
	terminals    map[string]terminalTask
	watching     bool
	terminalMu   sync.Mutex
	// sawUsage: the live thread reported its usage, so a restored one would
	// be older than what the client already has.
	sawUsage bool
}

var _ backend.SessionHandle = (*Session)(nil)

func newSession(p *process, result threadResponse) *Session {
	s := makeSession(p, result, nil)
	s.out = make(chan backend.Event, 2048)
	s.children = make(map[string]*Session)
	for _, t := range result.Thread.Turns {
		if t.Status == "inProgress" {
			s.startTurn(t.ID)
		}
	}
	return s
}

func makeSession(p *process, result threadResponse, parent *Session) *Session {
	s := &Session{
		p: p, id: result.Thread.ID, cwd: result.Thread.Cwd, mu: new(sync.Mutex), parent: parent,
		model: result.Model, provider: result.Provider, effort: result.Effort,
		deltaItems: make(map[string]bool), published: make(map[string]bool), pending: make(map[string]approval), inputs: make(map[string]pendingInput),
		plans: make(map[string]string), terminals: make(map[string]terminalTask),
	}
	if parent != nil {
		s.mu = parent.mu
	}
	if s.cwd == "" {
		s.cwd = result.Cwd
	}
	if s.provider == "" {
		s.provider = result.Thread.Provider
	}
	if result.Collaboration != nil {
		s.mode = result.Collaboration.Mode
	}
	if result.Profile != nil {
		s.profile = result.Profile.ID
	}
	return s
}

func (s *Session) NativeID() string             { return s.id }
func (s *Session) Events() <-chan backend.Event { return s.out }

func (s *Session) Send(ctx context.Context, blocks []acp2.ContentBlock) (acp2.MessageID, error) {
	input, err := promptInput(blocks)
	if err != nil {
		return "", err
	}
	s.ops.Lock()
	defer s.ops.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	id := acp2.MessageID(rand.Text())
	// Once sent, collect the result even if the ACP connection goes away.
	// The Codex turn and the event registry outlive that connection.
	ctx = context.WithoutCancel(ctx)
	turnID, err := s.submit(ctx, id, blocks, input)
	var rpc *rpcError
	if err != nil && turnID != "" && errors.As(err, &rpc) {
		s.mu.Lock()
		ended := s.turnID == ""
		s.mu.Unlock()
		if ended {
			// The acknowledged turn ended before Codex processed turn/steer.
			// An RPC error confirms the message was rejected, so start it once.
			_, err = s.submit(ctx, id, blocks, input)
		}
	}
	if err != nil {
		return "", threadError(s.id, err)
	}
	return id, nil
}

func (s *Session) submit(ctx context.Context, id acp2.MessageID, blocks []acp2.ContentBlock, input []userInput) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", &backend.SessionNotFoundError{ID: s.id}
	}
	previous := s.turnID
	s.submitting = true
	params := object{"threadId": s.id, "input": input, "clientUserMessageId": id}
	if previous == "" {
		if s.model != "" {
			params["model"] = s.model
		}
		if s.effort != "" {
			params["effort"] = s.effort
		}
		if s.sendMode && s.model != "" {
			var instructions *string
			if s.mode == "default" && s.instructions != "" {
				instructions = &s.instructions
			}
			var effort *string
			if s.effort != "" {
				effort = &s.effort
			}
			params["collaborationMode"] = object{"mode": s.mode, "settings": object{
				"model": s.model, "reasoning_effort": effort, "developer_instructions": instructions,
			}}
		}
	} else {
		params["expectedTurnId"] = previous
	}
	s.mu.Unlock()

	var result struct {
		Turn turn `json:"turn"`
	}
	method := "turn/start"
	if previous != "" {
		method = "turn/steer"
	}
	err := s.p.call(ctx, method, params, &result)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitting = false
	staged := s.staged
	s.staged = nil
	if s.closed {
		return previous, &backend.SessionNotFoundError{ID: s.id}
	}
	if err == nil {
		s.emit(update(schema.SessionUpdateUserMessage{MessageID: id, Content: optional.Of(blocks)}))
		s.startTurn(result.Turn.ID)
		s.published[string(id)] = true
	}
	for _, m := range staged {
		s.handle(m)
	}
	return previous, err
}

func (s *Session) Cancel(ctx context.Context) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	_, err := s.interrupt(ctx)
	return err
}

func (s *Session) interrupt(ctx context.Context) (<-chan struct{}, error) {
	s.mu.Lock()
	id, done := s.turnID, s.turnDone
	s.mu.Unlock()
	if id == "" {
		return nil, nil
	}
	params := object{"threadId": s.id, "turnId": id}
	retryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		err := s.p.call(retryCtx, "turn/interrupt", params, nil)
		if err == nil {
			return done, nil
		}
		var rpc *rpcError
		if !errors.As(err, &rpc) || rpc.Message != "no active turn to interrupt" {
			select {
			case <-done:
				return done, nil
			default:
				return done, err
			}
		}
		// turn/start can acknowledge a turn just before the runtime marks it
		// interruptible. Retry that specific response while this turn exists.
		timer := time.NewTimer(40 * time.Millisecond)
		select {
		case <-done:
			timer.Stop()
			return done, nil
		case <-retryCtx.Done():
			timer.Stop()
			return done, err
		case <-timer.C:
		}
	}
}

func (s *Session) Close(ctx context.Context) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	defer s.end(nil)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rpcTimeout)
	defer cancel()
	done, err := s.interrupt(ctx)
	if err == nil && done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	err = errors.Join(err, s.stopChildren(ctx), s.stopTerminals(ctx))
	unsubscribe := s.p.call(ctx, "thread/unsubscribe", object{"threadId": s.id}, nil)
	var missing *backend.SessionNotFoundError
	if errors.As(threadError(s.id, unsubscribe), &missing) {
		unsubscribe = nil
	}
	return errors.Join(err, unsubscribe)
}

func (s *Session) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endLocked(err)
}

func (s *Session) endLocked(err error) {
	if s.closed {
		return
	}
	if s.parent == nil {
		s.closed = true
		for _, child := range s.children {
			child.endLocked(err)
		}
	} else {
		s.running = false
		for _, id := range sortedIDs(s.terminals) {
			s.terminalEnded(id, optional.Of(schema.ToolCallStatusCancelled))
		}
		s.state(backend.TurnIdle, "", err)
		s.closed = true
	}
	s.p.remove(s)
	s.clearApprovals("cancel")
	s.clearInputs()
	if s.turnDone != nil {
		close(s.turnDone)
		s.turnDone = nil
	}
	if s.parent != nil {
		return
	}
	if err != nil {
		// emit reserves this slot so a failed session always reports its end.
		s.out <- backend.Event{Kind: backend.EventFatal, Err: err}
	}
	close(s.out)
}

func (s *Session) emit(ev backend.Event) {
	root := s.root()
	if s.closed || root.closed {
		return
	}
	if len(root.out) == cap(root.out)-1 {
		root.endLocked(errors.New("Codex session event queue exhausted"))
		return
	}
	if s != root {
		ev.Session = s.id
		if ev.Kind == backend.EventPermission {
			ev.Permission.Session = s.id
		}
	}
	root.out <- ev
}

func update[T schema.SessionUpdateVariants](v T) backend.Event {
	return backend.Event{Kind: backend.EventUpdate, Update: schema.NewSessionUpdate(v)}
}

func (s *Session) state(state backend.TurnState, reason acp2.StopReason, err error) {
	s.last = backend.TurnEvent{State: state, StopReason: reason, Err: err}
	s.publishState()
}

func (s *Session) startTurn(id string) {
	if s.closed || id == "" || id == s.turnID {
		return
	}
	if s.turnDone != nil {
		close(s.turnDone)
	}
	s.turnID = id
	s.fresh = false
	s.turnDone = make(chan struct{})
	clear(s.deltaItems)
	clear(s.plans)
	s.running = true
	s.state(backend.TurnRunning, "", nil)
}

func (s *Session) finishTurn(t turn) {
	if t.ID == "" || t.ID != s.turnID {
		return
	}
	s.clearApprovals("cancel")
	s.clearInputs()
	s.turnID = ""
	close(s.turnDone)
	s.turnDone = nil
	clear(s.deltaItems)
	clear(s.plans)
	clear(s.published)
	s.running = false
	end := turnEnd(t)
	s.state(end.State, end.StopReason, end.Err)
	s.watchTerminals()
}

func (s *Session) receive(m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		if len(m.ID) > 0 {
			s.p.respond(m.ID, nil, &rpcError{Code: -32601, Message: "Session is closed"})
		}
		return
	}
	if s.submitting {
		if len(s.staged) >= cap(s.root().out) {
			s.endLocked(errors.New("Codex turn setup event queue exhausted"))
		} else {
			s.staged = append(s.staged, m)
		}
		return
	}
	s.handle(m)
}

func (s *Session) handle(m message) {
	if s.closed {
		return
	}
	if len(m.ID) > 0 {
		s.request(m)
		return
	}
	var event notification
	if json.Unmarshal(m.Params, &event) != nil {
		return
	}
	switch m.Method {
	case "turn/started":
		s.startTurn(event.Turn.ID)
	case "turn/completed":
		s.finishTurn(event.Turn)
	case "thread/closed", "thread/deleted":
		s.endLocked(nil)
	case "serverRequest/resolved":
		key := string(event.RequestID)
		wasBlocked := s.waitingForUser()
		if approval, ok := s.pending[key]; ok {
			approval.timer.Stop()
			delete(s.pending, key)
		}
		if input, ok := s.inputs[key]; ok {
			s.removeInput(key, input)
		}
		s.syncUserAction(wasBlocked)

	default:
		s.translate(m.Method, event)
	}
}

type approval struct {
	id          json.RawMessage
	method      string
	permissions json.RawMessage
	decisions   []string
	timer       *time.Timer
}

func (s *Session) request(m message) {
	if m.Method == "item/tool/requestUserInput" {
		s.requestUserInput(m)
		return
	}
	var req struct {
		ItemID      string            `json:"itemId"`
		TurnID      string            `json:"turnId"`
		Command     string            `json:"command"`
		Reason      string            `json:"reason"`
		Permissions json.RawMessage   `json:"permissions"`
		Decisions   []json.RawMessage `json:"availableDecisions"`
		Network     *struct {
			Host     string `json:"host"`
			Protocol string `json:"protocol"`
		} `json:"networkApprovalContext"`
	}
	if json.Unmarshal(m.Params, &req) != nil {
		s.p.respond(m.ID, nil, &rpcError{Code: -32602, Message: "Invalid Codex request"})
		return
	}
	var name, title string
	switch m.Method {
	case "item/commandExecution/requestApproval":
		name, title = "commandExecution", req.Command
		if req.Network != nil {
			title = fmt.Sprintf("Network access to %s (%s)", req.Network.Host, req.Network.Protocol)
		}
	case "item/fileChange/requestApproval":
		name, title = "fileChange", "Apply file changes"
	case "item/permissions/requestApproval":
		name, title = "permissions", "Grant additional permissions"
		if len(req.Permissions) > 0 {
			title += "\n" + string(req.Permissions)
		}
	default:
		s.p.respond(m.ID, nil, &rpcError{Code: -32601, Message: "Unsupported Codex server request"})
		return
	}
	if title == "" {
		title = "Run command"
	}
	if req.Reason != "" {
		title += "\n" + req.Reason
	}
	var decisions []string
	for _, raw := range req.Decisions {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			decisions = append(decisions, value)
		}
	}
	key := string(m.ID)
	timer := time.AfterFunc(2*time.Minute, func() { _ = s.Respond(key, backend.Decision{}) })
	wasBlocked := s.waitingForUser()
	s.pending[key] = approval{id: m.ID, method: m.Method, permissions: req.Permissions, decisions: decisions, timer: timer}
	if req.TurnID != "" && s.turnID == "" {
		s.startTurn(req.TurnID)
	}
	s.syncUserAction(wasBlocked)
	s.emit(backend.Event{Kind: backend.EventPermission, Permission: backend.PermissionRequest{
		ReqID: key, ToolCallID: req.ItemID, Title: title, ToolName: name,
	}})
}

func (s *Session) Respond(key string, d backend.Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.respondLocked(key, d) {
		return nil
	}
	if s.parent == nil {
		for _, child := range s.children {
			if child.respondLocked(key, d) {
				break
			}
		}
	}
	return nil
}

func (s *Session) respondLocked(key string, d backend.Decision) bool {
	req, ok := s.pending[key]
	if !ok {
		return false
	}
	delete(s.pending, key)
	req.timer.Stop()
	decision := "decline"
	if d.Cancelled {
		decision = "cancel"
	} else if d.Allow {
		decision = "accept"
	}
	s.replyApproval(req, decision)
	s.syncUserAction(true)
	return true
}

func (s *Session) clearApprovals(decision string) {
	for key, req := range s.pending {
		req.timer.Stop()
		delete(s.pending, key)
		s.replyApproval(req, decision)
	}
}

func (s *Session) replyApproval(req approval, decision string) {
	if req.method == "item/permissions/requestApproval" {
		permissions := json.RawMessage("{}")
		if decision == "accept" && len(req.permissions) > 0 {
			permissions = req.permissions
		}
		s.p.respond(req.id, object{"permissions": permissions, "scope": "turn"}, nil)
		return
	}
	if len(req.decisions) > 0 && !slices.Contains(req.decisions, decision) {
		decision = "cancel"
		for _, option := range req.decisions {
			if option == "decline" {
				decision = "decline"
				break
			}
		}
	}
	s.p.respond(req.id, object{"decision": decision}, nil)
}
