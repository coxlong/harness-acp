package claude

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// The session side of background work: following a background command's output
// file, ending its terminal, and repeating the foreground state whenever the
// set of background work changes.

// tailer follows one terminal's output file until the task ends.
type tailer struct{ stop chan stopJob }

const tailInterval = 500 * time.Millisecond

// tailLimit bounds what one poll reads; a larger backlog arrives over several polls.
const tailLimit = 1 << 20

// afterLine acts on what a line asked of the session: output files to follow,
// terminals to end, and a changed set of background work. Callers hold s.mu.
func (s *Session) afterLine() {
	tails, stops := s.tr.takeJobs("")
	for _, j := range tails {
		s.startTail(j)
	}
	for _, j := range stops {
		s.stopTerminal(j)
	}
	s.refreshBackground()
}

func (s *Session) startTail(j tailJob) {
	if s.tailers == nil {
		s.tailers = map[string]*tailer{}
	}
	tl := &tailer{stop: make(chan stopJob, 1)}
	s.tailers[j.terminal] = tl
	go s.runTail(j, tl)
}

// stopTerminal ends a terminal: its tail flushes the rest of the output and
// then reports the exit. A terminal with no tail exits at once.
func (s *Session) stopTerminal(j stopJob) {
	if tl := s.tailers[j.terminal]; tl != nil {
		delete(s.tailers, j.terminal)
		tl.stop <- j
		return
	}
	s.send(exitEvent(j))
}

// exitEvent reports a terminal's exit, which also withdraws the cancel it advertised.
func exitEvent(j stopJob) backend.Event {
	u := schema.SessionUpdateTerminalUpdate{TerminalID: terminalID(j.terminal), ExitStatus: optional.Of(j.exit)}
	if j.cancellable {
		u.Capabilities = optional.Null[schema.TerminalCapabilities]()
	}
	return childEvent(j.session, terminalUpdate(u))
}

// runTail sends a background command's output as it is written: first a
// snapshot, which replaces the launch notice in the terminal, then chunks.
func (s *Session) runTail(j tailJob, tl *tailer) {
	var off int64
	sent := false
	poll := func() {
		data, next, reset := readNew(j.path, off)
		off = next
		if len(data) == 0 && !reset {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !sent || reset {
			sent = true
			s.send(childEvent(j.session, terminalUpdate(schema.SessionUpdateTerminalUpdate{
				TerminalID: terminalID(j.terminal), Output: optional.Of(schema.TerminalOutput{Data: encodeTerminal(string(data))}),
			})))
			return
		}
		s.send(childEvent(j.session, update(schema.SessionUpdateTerminalOutputChunk{TerminalID: terminalID(j.terminal), Data: encodeTerminal(string(data))})))
	}
	tick := time.NewTicker(tailInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			poll()
		case end := <-tl.stop:
			poll()
			s.mu.Lock()
			if !sent { // the notice still shows: replace it, even with nothing
				s.send(childEvent(j.session, terminalUpdate(schema.SessionUpdateTerminalUpdate{
					TerminalID: terminalID(j.terminal), Output: optional.Of(schema.TerminalOutput{}),
				})))
			}
			s.send(exitEvent(end))
			s.mu.Unlock()
			return
		case <-s.done:
			return
		}
	}
}

// readNew reads a file from off. reset reports that the file shrank, so the
// reader started again from its beginning.
func readNew(path string, off int64) (data []byte, next int64, reset bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, off, false // not written yet
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < off {
		off, reset = 0, true
	}
	buf := make([]byte, tailLimit)
	n, _ := f.ReadAt(buf, off)
	if n < 0 || (n == 0 && err == io.EOF) {
		n = 0
	}
	return buf[:n], off + int64(n), reset
}

// refreshBackground repeats the foreground state, without a stop reason, when
// the set of background work changed: the work is part of every state.
func (s *Session) refreshBackground() {
	if !s.tr.bgSeen {
		return
	}
	// Reports the change makes before the state that names the work: a tool
	// call started for a task, a terminal or call that can be cancelled says so,
	// and a subagent's own list.
	for _, ev := range s.tr.backgroundEvents("") {
		s.send(ev)
	}
	bg := s.tr.backgroundWork()
	key, _ := json.Marshal(bg)
	if string(key) == s.bgKey {
		return
	}
	s.bgKey = string(key)
	s.send(backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{
		State: s.stateLocked(), Background: bg,
	}})
}

// stateLocked is the session's foreground state right now.
func (s *Session) stateLocked() backend.TurnState {
	switch {
	case !s.foreground:
		return backend.TurnIdle
	case len(s.pending) > 0:
		return backend.TurnRequiresAction
	}
	return backend.TurnRunning
}

// CancelChild stops the subagent whose child session is id, as session/cancel
// of that session asks: Claude Code stops the subagent's work and the
// foreground subagents it waits on, and the parent goes on. Nothing is reported
// here: accepting the stop is no evidence the subagent stopped, so its idle
// (cancelled) follows from the task events Claude Code sends. A subagent that
// already ended keeps its outcome. It returns false when id is not a child
// session of this session.
func (s *Session) CancelChild(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	c := s.tr.childByID(id)
	known := c != nil
	working := known && !c.ended
	s.mu.Unlock()
	if !known {
		return false, nil
	}
	if !working {
		return true, nil
	}
	p := s.rt.Get(s.uuid)
	if p == nil {
		return true, nil // no process: nothing runs
	}
	// Claude Code does not answer a stop_task: the task events report how it ends.
	return true, s.control(p, map[string]any{"subtype": "stop_task", "task_id": id})
}

// CancelTerminal stops the background command whose terminal is terminal, in
// the session sessionID (this one, or a subagent's child session), as
// terminal/cancel asks. The exit follows from Claude Code's task events. It
// returns false when no such command runs in the background.
func (s *Session) CancelTerminal(ctx context.Context, sessionID, terminal string) (found bool, err error) {
	s.mu.Lock()
	taskID, ok := s.tr.terminalTask(sessionID, s.uuid, terminal)
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	p := s.rt.Get(s.uuid)
	if p == nil {
		return false, nil
	}
	return true, s.control(p, map[string]any{"subtype": "stop_task", "task_id": taskID})
}

// CancelToolCall stops the background task that the tool call toolCall stands
// for, in the session sessionID (this one, or a subagent's child session), as
// tool_call/cancel asks. The call's end follows from Claude Code's task events.
// It returns false when no listed call has that id.
func (s *Session) CancelToolCall(ctx context.Context, sessionID, toolCall string) (found bool, err error) {
	s.mu.Lock()
	taskID, ok := s.tr.callTask(sessionID, s.uuid, toolCall)
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	p := s.rt.Get(s.uuid)
	if p == nil {
		return false, nil
	}
	return true, s.control(p, map[string]any{"subtype": "stop_task", "task_id": taskID})
}

// ChildWorking reports whether the subagent whose child session is id works now.
func (s *Session) ChildWorking(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.tr.childByID(id)
	return c != nil && !c.ended
}

// OwnsSession reports whether id is this session or one of its subagents'.
func (s *Session) OwnsSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id == s.uuid || s.tr.childByID(id) != nil
}

// syncBlockedChildren reports a subagent as waiting for the user while a
// permission request of its own is open, and running again once none is.
func (s *Session) syncBlockedChildren() {
	open := map[string]bool{}
	for _, pp := range s.pending {
		if pp.child != "" {
			open[pp.child] = true
		}
	}
	for _, c := range s.tr.children() {
		if b := open[c.id]; b != c.blocked {
			c.blocked = b
			if c.ended {
				continue
			}
			for _, ev := range s.tr.childState(c) {
				s.send(ev)
			}
		}
	}
}
