package codex

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func (s *Session) root() *Session {
	for s.parent != nil {
		s = s.parent
	}
	return s
}

func (i item) childIDs() []string {
	path := strings.Trim(strings.TrimSpace(i.AgentPath), "/")
	switch {
	case i.Type == "collabAgentToolCall" && i.Tool == "spawnAgent":
		return i.Receivers
	case i.Type == "subAgentActivity" && path != "" && path != "root":
		return []string{i.AgentThreadID}
	}
	return nil
}

func (s *Session) addChild(id string, t thread) *Session {
	if id == "" || s.closed || s.root().closed {
		return nil
	}
	root := s.root()
	if child := root.children[id]; child != nil {
		if child.parent != s {
			return nil
		}
		if !child.closed {
			if label := childLabel(t); label != "" && child.label != label {
				child.label = label
				s.emit(update(child.association()))
			}
			return child
		}
		if t.ID != id {
			return nil
		}
	}
	t.ID = id
	if t.Cwd == "" {
		t.Cwd = s.cwd
	}
	child := makeSession(s.p, threadResponse{Thread: t}, s)
	s.p.mu.Lock()
	if s.p.failure != nil || s.p.sessions[id] != nil || s.p.children[id] != nil {
		s.p.mu.Unlock()
		return nil
	}
	s.p.children[id] = child
	s.p.mu.Unlock()
	root.children[id] = child
	child.label = childLabel(t)
	child.running = true
	child.last.State = backend.TurnRunning
	child.publishState()
	return child
}

func childLabel(t thread) string {
	for _, label := range []string{t.Nickname, t.Role, t.Name} {
		if label != "" {
			return label
		}
	}
	return ""
}

func (s *Session) observeChildren(i item) {
	if i.SenderThreadID == "" || i.SenderThreadID == s.id {
		for _, id := range i.childIDs() {
			var t thread
			if i.Type == "subAgentActivity" && i.Kind == "started" {
				t.ID = id
			}
			s.addChild(id, t)
		}
	}
	if i.Type == "subAgentActivity" {
		switch i.Kind {
		case "started", "resumed":
			s.childStatus(i.AgentThreadID, "running")
		case "completed":
			s.childStatus(i.AgentThreadID, "completed")
		case "interrupted":
			s.childStatus(i.AgentThreadID, "interrupted")
		}
	}
	for id, status := range i.AgentsStates {
		s.childStatus(id, status.Status)
	}
}

func (s *Session) childStatus(id, status string) {
	child := s.root().children[id]
	if child == nil || child.closed || child.parent != s || child.turnID != "" {
		return
	}
	switch status {
	case "pendingInit", "running":
		if !child.running {
			child.running = true
			child.state(backend.TurnRunning, "", nil)
		}
	case "completed", "interrupted", "errored", "shutdown", "notFound":
		if child.running {
			child.running = false
			if status == "errored" {
				status = "failed"
			}
			if status == "shutdown" || status == "notFound" {
				status = "interrupted"
			}
			end := turnEnd(turn{Status: status})
			child.state(end.State, end.StopReason, end.Err)
		}
	}
}

func (s *Session) association() schema.SessionUpdateSubagentUpdate {
	label := s.label
	if label == "" {
		label = "Codex subagent"
	}
	end := s.last
	end.Background = s.background()
	u := schema.SessionUpdateSubagentUpdate{
		SessionID: schema.SessionID(s.id), Title: optional.Of(label),
		State: optional.Of(childState(end)),
	}
	caps := schema.SubagentSessionCapabilities{}
	if s.running {
		caps.Cancel = &schema.SessionCancelCapabilities{}
	}
	u.Capabilities = optional.Of(caps)
	if s.parent != nil && s.cwd != s.parent.cwd {
		if dir := fullPath("", s.cwd); dir != "" {
			u.Cwd = optional.Of(dir)
		}
	}
	return u
}

func childState(t backend.TurnEvent) schema.StateUpdate {
	bg := t.Background
	switch {
	case t.State == backend.TurnRunning:
		return schema.NewStateUpdate(schema.StateUpdateRunning{Background: bg})
	case t.State == backend.TurnRequiresAction:
		return schema.NewStateUpdate(schema.StateUpdateRequiresAction{Background: bg})
	case t.Err != nil:
		return schema.NewStateUpdate(schema.StateUpdateIdle{Value: schema.NewIdleStateUpdate(schema.IdleStateUpdateError{
			Background: bg, Error: &schema.Error{Code: -32603, Message: t.Err.Error()},
		})})
	case t.StopReason == acp2.StopReasonCancelled:
		return schema.NewStateUpdate(schema.StateUpdateIdle{Value: schema.NewIdleStateUpdate(schema.IdleStateUpdateCancelled{Background: bg})})
	case t.StopReason == acp2.StopReasonEndTurn:
		return schema.NewStateUpdate(schema.StateUpdateIdle{Value: schema.NewIdleStateUpdate(schema.IdleStateUpdateEndTurn{Background: bg})})
	default:
		return schema.NewStateUpdate(schema.StateUpdateIdle{Value: schema.NewIdleStateUpdate(schema.IdleStateUpdateUntagged{Background: bg})})
	}
}

func turnEnd(t turn) backend.TurnEvent {
	switch t.Status {
	case "interrupted":
		return backend.TurnEvent{StopReason: acp2.StopReasonCancelled}
	case "failed":
		msg := "Codex turn failed"
		if t.Error != nil && t.Error.Message != "" {
			msg = t.Error.Message
		}
		return backend.TurnEvent{Err: errors.New(msg)}
	case "completed":
		return backend.TurnEvent{StopReason: acp2.StopReasonEndTurn}
	}
	return backend.TurnEvent{}
}

func (s *Session) background() []acp2.BackgroundWork {
	bg := []acp2.BackgroundWork{}
	for _, id := range sortedIDs(s.terminals) {
		bg = append(bg, schema.NewBackgroundWork(schema.BackgroundWorkTerminal{TerminalID: schema.TerminalID(id)}))
	}
	children := s.root().children
	for _, id := range sortedIDs(children) {
		c := children[id]
		if c.parent == s && !c.closed && (c.running || len(c.terminals) > 0) {
			bg = append(bg, schema.NewBackgroundWork(schema.BackgroundWorkSubagent{SessionID: schema.SessionID(c.id)}))
		}
	}
	return bg
}

func sortedIDs[T any](entries map[string]T) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (s *Session) publishState() {
	if s.parent != nil {
		s.parent.emit(update(s.association()))
	}
	event := s.last
	event.Background = s.background()
	s.emit(backend.Event{Kind: backend.EventTurnState, Turn: event})
	for parent := s.parent; parent != nil; parent = parent.parent {
		parent.emit(backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{
			State: parent.last.State, Background: parent.background(),
		}})
	}
}

func (s *Session) ChildWorking(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	child := s.root().children[id]
	return child != nil && !child.closed && (child.running || len(child.terminals) > 0)
}

func (s *Session) OwnsSession(id string) bool {
	return s.owned(id) != nil
}

func (s *Session) owned(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.id && !s.closed {
		return s
	}
	if child := s.root().children[id]; child != nil && !child.closed {
		return child
	}
	return nil
}

func (s *Session) CancelChild(ctx context.Context, id string) (bool, error) {
	child := s.owned(id)
	if child == nil || child == s {
		return false, nil
	}
	return true, child.cancelActive(ctx)
}

func (s *Session) cancelActive(ctx context.Context) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	s.mu.Lock()
	findTurn := s.turnID == "" && s.running && !s.closed
	s.mu.Unlock()
	if findTurn {
		var result struct {
			Data []turn `json:"data"`
		}
		err := s.p.call(ctx, "thread/turns/list", object{"threadId": s.id, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &result)
		if err != nil {
			return threadError(s.id, err)
		}
		s.mu.Lock()
		if s.turnID == "" && s.running && len(result.Data) != 0 && result.Data[0].Status == "inProgress" {
			s.startTurn(result.Data[0].ID)
		}
		s.mu.Unlock()
	}
	_, err := s.interrupt(ctx)
	return err
}

func (s *Session) stopChildren(ctx context.Context) error {
	s.mu.Lock()
	children := make([]*Session, 0, len(s.children))
	for _, child := range s.children {
		if !child.closed {
			children = append(children, child)
		}
	}
	s.mu.Unlock()
	var errs []error
	for _, child := range children {
		errs = append(errs, child.cancelActive(ctx), child.stopTerminals(ctx))
	}
	return errors.Join(errs...)
}
