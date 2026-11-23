package codex

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type terminalTask struct {
	ItemID    string `json:"itemId"`
	ProcessID string `json:"processId"`
	Command   string `json:"command"`
	Cwd       string `json:"cwd"`
}

func (s *Session) nativeTerminals(ctx context.Context) (map[string]terminalTask, error) {
	tasks := make(map[string]terminalTask)
	var cursor *string
	for {
		var page struct {
			Data       []terminalTask `json:"data"`
			NextCursor *string        `json:"nextCursor"`
		}
		if err := s.p.call(ctx, "thread/backgroundTerminals/list", object{"threadId": s.id, "cursor": cursor}, &page); err != nil {
			return nil, err
		}
		for _, task := range page.Data {
			if task.ItemID != "" && task.ProcessID != "" {
				tasks[task.ItemID] = task
			}
		}
		if page.NextCursor == nil {
			return tasks, nil
		}
		if cursor != nil && *cursor == *page.NextCursor {
			return nil, errors.New("Codex returned a repeated terminal cursor")
		}
		cursor = page.NextCursor
	}
}

func (s *Session) watchTerminals() {
	if s.watching || s.closed {
		return
	}
	s.watching = true
	go s.pollTerminals()
}

func (s *Session) pollTerminals() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.terminalMu.Lock()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		tasks, err := s.nativeTerminals(ctx)
		cancel()
		s.mu.Lock()
		if !s.closed && err == nil {
			s.updateTerminals(tasks)
		}
		var rpc *rpcError
		stop := s.closed || (s.turnID == "" && !s.running && len(s.terminals) == 0) ||
			(errors.As(err, &rpc) && rpc.Code == -32601)
		if stop {
			s.watching = false
		}
		s.mu.Unlock()
		s.terminalMu.Unlock()
		if stop {
			return
		}
		select {
		case <-s.p.done:
			return
		case <-ticker.C:
		}
	}
}

func (s *Session) updateTerminals(tasks map[string]terminalTask) {
	changed := false
	for _, id := range sortedIDs(s.terminals) {
		if _, ok := tasks[id]; !ok {
			s.terminalEnded(id, optional.Of(schema.ToolCallStatusCompleted))
			changed = true
		}
	}
	for _, id := range sortedIDs(tasks) {
		task := tasks[id]
		if s.terminals[id] == task {
			continue
		}
		s.terminals[id] = task
		changed = true
		for _, ev := range task.events() {
			s.emit(ev)
		}
	}
	if changed {
		s.publishState()
	}
}

func (t terminalTask) events() []backend.Event {
	tid := schema.TerminalID(t.ItemID)
	terminal := schema.SessionUpdateTerminalUpdate{
		TerminalID: tid, Command: optional.Of(t.Command), ExitStatus: optional.Null[schema.TerminalExitStatus](),
		Capabilities: optional.Of(schema.TerminalCapabilities{Cancel: &schema.TerminalCancelCapabilities{}}),
	}
	if filepath.IsAbs(t.Cwd) {
		terminal.Cwd = optional.Of(schema.AbsolutePath(t.Cwd))
	}
	return []backend.Event{update(terminal), update(schema.SessionUpdateToolCallUpdate{
		ToolCallID: schema.ToolCallID(t.ItemID), Title: optional.Of(t.Command),
		Kind: optional.Of(schema.ToolKindExecute), Status: optional.Of(schema.ToolCallStatusInProgress),
		Capabilities: optional.Of(schema.ToolCallCapabilities{Cancel: &schema.ToolCallCancelCapabilities{}}),
		Content:      optional.Of([]schema.ToolCallContent{schema.NewToolCallContent(schema.ToolCallContentTerminal{TerminalID: tid})}),
	})}
}

func (s *Session) terminalEnded(id string, status optional.Value[schema.ToolCallStatus]) {
	delete(s.terminals, id)
	s.emit(update(schema.SessionUpdateTerminalUpdate{
		TerminalID: schema.TerminalID(id), Capabilities: optional.Of(schema.TerminalCapabilities{}),
		ExitStatus: optional.Of(schema.TerminalExitStatus{}),
	}))
	s.emit(update(schema.SessionUpdateToolCallUpdate{
		ToolCallID: schema.ToolCallID(id), Status: status,
		Capabilities: optional.Of(schema.ToolCallCapabilities{}),
	}))
}

func (s *Session) CancelTerminal(ctx context.Context, sessionID, id string) (bool, error) {
	return s.cancelTerminal(ctx, sessionID, id, optional.Value[schema.ToolCallStatus]{})
}

func (s *Session) CancelToolCall(ctx context.Context, sessionID, id string) (bool, error) {
	return s.cancelTerminal(ctx, sessionID, id, optional.Of(schema.ToolCallStatusCancelled))
}

func (s *Session) cancelTerminal(ctx context.Context, sessionID, id string, status optional.Value[schema.ToolCallStatus]) (bool, error) {
	target := s.owned(sessionID)
	if target == nil {
		return false, nil
	}
	return target.stopTerminal(ctx, id, status)
}

func (s *Session) stopTerminal(ctx context.Context, id string, status optional.Value[schema.ToolCallStatus]) (bool, error) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	s.mu.Lock()
	known := s.terminals[id]
	s.mu.Unlock()
	tasks, err := s.nativeTerminals(ctx)
	if err != nil {
		return known.ProcessID != "", threadError(s.id, err)
	}
	task, found := tasks[id]
	if !found {
		return false, nil
	}
	terminated, err := s.terminate(ctx, task)
	if err != nil || !terminated {
		return err != nil, err
	}
	s.mu.Lock()
	s.terminalEnded(id, status)
	s.publishState()
	s.mu.Unlock()
	return true, nil
}

func (s *Session) terminate(ctx context.Context, task terminalTask) (bool, error) {
	var result struct {
		Terminated bool `json:"terminated"`
	}
	err := s.p.call(ctx, "thread/backgroundTerminals/terminate", object{
		"threadId": s.id, "processId": task.ProcessID,
	}, &result)
	return result.Terminated, threadError(s.id, err)
}

func (s *Session) stopTerminals(ctx context.Context) error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	tasks, err := s.nativeTerminals(ctx)
	var rpc *rpcError
	var missing *backend.SessionNotFoundError
	if errors.As(err, &rpc) && rpc.Code == -32601 || errors.As(threadError(s.id, err), &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, task := range tasks {
		_, err := s.terminate(ctx, task)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
