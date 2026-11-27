package claude

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Session is one Claude Code conversation. It implements backend.SessionHandle.
//
// Turn model (mirrors claude-agent-acp's v2 session): a turn is open from the
// first prompt (or the first main-conversation output of a cycle Claude Code
// started itself) until Claude Code's `result`. Prompts that arrive while a
// turn is open are written to stdin at once; Claude Code folds or queues them.
// The session reports running / requires_action / idle(stopReason) as it goes.
type Session struct {
	uuid string
	rt   *Runtime

	events chan backend.Event

	// mu serialises every state change together with the events it produces,
	// so a client sees user_message → running → output → idle in order. It is
	// held across channel sends; Close unblocks them via done.
	mu               sync.Mutex
	opts             SpawnOpts // opts.New flips to false after the first spawn
	tr               *translator
	foreground       bool // a turn is open
	cancelling       bool // session/cancel hit the open turn; its idle reports cancelled
	pending          map[string]pendingPerm
	info             *initInfo          // what Claude Code offers; set by Init
	terminalCommands []string           // CLI terminal-bound slash commands from system/init
	tailers          map[string]*tailer // by terminal id
	bgKey            string             // the background work last reported, as JSON
	titled           bool               // a session title went out
	titling          bool               // a title request is out
	reqSeq           atomic.Uint64

	startedWork bool // foreground work has started on this handle

	// ctl holds the control requests that wait for an answer.
	ctlMu sync.Mutex
	ctl   map[string]chan ctlReply

	// closeMu guards closed/events so Close can close the channel safely.
	closeMu sync.RWMutex
	closed  bool
	done    chan struct{}
	once    sync.Once
}

type pendingPerm struct {
	p     *proc
	input map[string]any
	child string // the subagent session the request belongs to
}

// NewSession wraps a session uuid. The process is spawned on first Send.
func NewSession(uuid string, rt *Runtime, opts SpawnOpts) *Session {
	tr := newTranslator()
	tr.sessionID, tr.cwd = uuid, opts.Cwd
	return &Session{
		uuid:    uuid,
		rt:      rt,
		opts:    opts,
		events:  make(chan backend.Event, 256),
		tr:      tr,
		pending: make(map[string]pendingPerm),
		done:    make(chan struct{}),
	}
}

// NativeID returns the Claude session UUID.
func (s *Session) NativeID() string { return s.uuid }

// Events returns the session's output. It is closed by Close.
func (s *Session) Events() <-chan backend.Event { return s.events }

// send delivers one event. Callers hold s.mu, which keeps events ordered.
func (s *Session) send(ev backend.Event) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (s *Session) turn(state backend.TurnState, reason acp2.StopReason, err error) {
	if state == backend.TurnRunning {
		s.startedWork = true
	}
	s.send(backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{
		State: state, StopReason: reason, Err: err, Background: s.tr.backgroundWork(),
	}})
}

// --- input ------------------------------------------------------------------

// Send writes a prompt to claude. It returns once the prompt is accepted (v2:
// the turn's end is reported by idle, not by this call).
func (s *Session) Send(ctx context.Context, blocks []acp2.ContentBlock) (acp2.MessageID, error) {
	msgID := newUUID()
	line, err := buildUserMessage(s.uuid, msgID, blocks)
	if err != nil {
		return "", err
	}
	p, err := s.ensureProc()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.opts.New = false // the first prompt creates the transcript: later starts resume it
	s.mu.Unlock()

	// Reported before the write: Claude Code's output can follow the write at
	// once, and the client must see the prompt before the work it starts.
	s.mu.Lock()
	startTurn := !s.foreground
	if startTurn {
		s.foreground = true
		s.cancelling = false
		s.tr.agentText = false
		s.tr.duplicateCompactErr = ""
		p.busy.Store(true)
	}
	s.send(userMessage(msgID, blocks))
	if startTurn {
		s.turn(backend.TurnRunning, "", nil)
	}
	s.mu.Unlock()

	if startTurn {
		s.askTitle(p, blocks)
	}

	// The write is outside mu on purpose: a large prompt (a base64 image)
	// can exceed the pipe buffer, and then it waits for Claude Code to read
	// while Claude Code may be waiting for readLoop, which needs mu.
	if err := p.write(line); err != nil {
		if startTurn {
			// Nothing reached Claude Code: end the turn that this prompt opened.
			s.mu.Lock()
			if s.foreground {
				s.foreground = false
				p.busy.Store(false)
				s.turn(backend.TurnIdle, "", fmt.Errorf("claude: write prompt: %w", err))
			}
			s.mu.Unlock()
		}
		return "", fmt.Errorf("claude: write prompt: %w", err)
	}
	return acp2.MessageID(msgID), nil
}

// ensureProc returns the live process, spawning (and starting a reader for) a
// new one if there is none: fresh sessions use --session-id, later ones --resume.
func (s *Session) ensureProc() (*proc, error) {
	s.mu.Lock()
	opts := s.opts
	s.mu.Unlock()

	p, fresh, err := s.rt.Acquire(s.uuid, opts)
	if err != nil {
		return nil, err
	}
	if fresh {
		go s.readLoop(p)
	}
	return p, nil
}

// Cancel interrupts the open turn. Claude Code then ends it with a `result`
// whose terminal_reason is aborted_*, which this session reports as cancelled.
func (s *Session) Cancel(ctx context.Context) error {
	p := s.rt.Get(s.uuid)
	if p == nil {
		return nil // no process: nothing is running
	}
	s.mu.Lock()
	if s.foreground {
		s.cancelling = true
	}
	s.mu.Unlock()
	return s.control(p, map[string]any{"subtype": "interrupt", "cancel_queued": true})
}

// control writes a control_request. The control_response for it is ignored.
func (s *Session) control(p *proc, body map[string]any) error {
	line, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("harnessd-%d", s.reqSeq.Add(1)),
		"request":    body,
	})
	if err != nil {
		return err
	}
	return p.write(line)
}

// Respond answers a pending can_use_tool request.
func (s *Session) Respond(reqID string, d backend.Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pp, ok := s.pending[reqID]
	if !ok {
		return fmt.Errorf("claude: no pending permission request %q", reqID)
	}
	delete(s.pending, reqID)

	// In v2 a "cancelled" answer is also the user dismissing the prompt. Only
	// while session/cancel is stopping the work does it abort the tool use;
	// otherwise it is a refusal and the turn goes on.
	if !d.Allow && s.cancelling {
		d.Cancelled = true
	}
	var resp any
	switch {
	case d.Cancelled:
		// The turn is being cancelled: abort the tool use rather than refuse it.
		resp = map[string]any{"subtype": "error", "request_id": reqID, "error": "Tool use aborted"}
	case d.Allow:
		resp = map[string]any{"subtype": "success", "request_id": reqID, "response": map[string]any{
			"behavior": "allow", "updatedInput": pp.input,
		}}
	default:
		resp = map[string]any{"subtype": "success", "request_id": reqID, "response": map[string]any{
			"behavior": "deny", "message": "User refused permission to run tool",
		}}
	}
	line, err := json.Marshal(map[string]any{"type": "control_response", "response": resp})
	if err != nil {
		return err
	}
	werr := pp.p.write(line)

	// The turn is blocked on the user only while a request is open.
	if len(s.pending) == 0 && s.foreground && !s.cancelling {
		s.turn(backend.TurnRunning, "", nil)
	}
	s.syncBlockedChildren()
	return werr
}

// Close ends the session: the process is killed and Events is closed.
func (s *Session) Close(ctx context.Context) error {
	s.once.Do(func() {
		close(s.done) // releases any send blocked on a full channel
		s.rt.Kill(s.uuid)
		s.closeMu.Lock()
		s.closed = true
		close(s.events)
		s.closeMu.Unlock()
	})
	return nil
}

// --- output -----------------------------------------------------------------

// readLoop reads one process's stdout until it ends, then reaps it.
func (s *Session) readLoop(p *proc) {
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024) // tool results can be large
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		s.handle(p, raw)
	}
	scanErr := sc.Err()
	p.reap() // stdout is drained: Wait is now safe
	s.procExited(p, scanErr)
}

// handle processes one stdout line. It holds s.mu for the whole line so state
// changes and the events they produce stay together and in order.
func (s *Session) handle(p *proc, raw []byte) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return // not a message we understand; claude may print diagnostics
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.afterLine()

	switch l.Type {
	case "stream_event", "assistant":
		s.outputStarts(p, &l)
		for _, ev := range s.tr.content(&l) {
			s.send(ev)
		}
	case "user":
		for _, ev := range s.tr.content(&l) {
			s.send(ev)
		}
	case "control_response":
		s.controlReply(raw)
	case "control_request":
		s.permissionRequested(p, &l)
	case "control_cancel_request":
		// Claude Code withdrew a request (the turn was interrupted, or a hook
		// answered it): stop waiting for the user.
		s.permissionWithdrawn(l.RequestID)
	case "system":
		switch l.Subtype {
		case "init":
			if len(l.TerminalSlashCommands) > 0 && !slices.Equal(s.terminalCommands, l.TerminalSlashCommands) {
				s.terminalCommands = slices.Clone(l.TerminalSlashCommands)
				if s.info != nil {
					s.send(update(schema.SessionUpdateAvailableCommandsUpdate{
						AvailableCommands: buildCommands(s.info, s.terminalCommands),
					}))
				}
			}
		case "commands_changed":
			if s.info == nil {
				s.info = &initInfo{}
			}
			s.info.Commands = l.Commands
			s.send(update(schema.SessionUpdateAvailableCommandsUpdate{
				AvailableCommands: buildCommands(s.info, s.terminalCommands),
			}))
		case "local_command_output":
			if l.Content != "" && !s.tr.consumeDuplicateCompactError(l.Content) {
				s.tr.agentText = true
				s.send(agentChunkEv(firstNonEmpty(l.UUID, newUUID()), l.Content))
			}
		default:
			for _, ev := range s.tr.system(&l) {
				s.send(ev)
			}
		}
	case "result":
		s.result(p, raw)
	}
}

// outputStarts opens a turn for main-conversation output that no prompt of
// ours started: a cycle Claude Code began itself, such as after a background
// task ended. Subagent output never opens a turn.
func (s *Session) outputStarts(p *proc, l *line) {
	if s.foreground || l.ParentToolUseID != nil {
		return
	}
	s.foreground = true
	s.cancelling = false
	s.tr.agentText = false
	s.tr.errKind, s.tr.errText = "", ""
	p.busy.Store(true)
	s.turn(backend.TurnRunning, "", nil)
}

func (s *Session) permissionRequested(p *proc, l *line) {
	ev, ok := s.tr.permission(l)
	if !ok {
		return // a control_request we do not serve (hooks, MCP): ignored
	}
	var req permissionRequest
	_ = json.Unmarshal(l.Request, &req)
	s.pending[ev.Permission.ReqID] = pendingPerm{p: p, input: req.Input, child: ev.Permission.Session}

	// Work blocked on the user is requires_action, whether or not a turn is open.
	if s.foreground {
		s.turn(backend.TurnRequiresAction, "", nil)
	}
	s.syncBlockedChildren()
	// A held SendMessage call that asks is shown: the client cannot be asked about a call it does not know.
	for _, shown := range s.tr.reveal(ev.Permission.ToolCallID) {
		s.send(shown)
	}
	s.send(ev)
}

func (s *Session) permissionWithdrawn(reqID string) {
	if _, ok := s.pending[reqID]; !ok {
		return
	}
	delete(s.pending, reqID)
	// After a cancel the work is stopping and idle follows: no running first.
	if len(s.pending) == 0 && s.foreground && !s.cancelling {
		s.turn(backend.TurnRunning, "", nil)
	}
	s.syncBlockedChildren()
}

// result ends the open turn.
func (s *Session) result(p *proc, raw []byte) {
	var r resultMsg
	if json.Unmarshal(raw, &r) != nil || r.isPlaceholder() {
		return
	}
	reason, err := r.end(s.cancelling)
	if err != nil {
		// A failed request left its own text and error kind in a synthetic message.
		text := firstNonEmpty(s.tr.errText, err.Error())
		err = failure(text, s.tr.errKind, r.Subtype)
	}

	// A local command (such as /context) answers with its result alone: no
	// model text streamed for it. Show that result as an agent message.
	if err == nil && reason == acp2.StopReasonEndTurn && r.Subtype == "success" &&
		!r.IsError && r.Result != "" && !s.tr.agentText {
		id := firstNonEmpty(r.UUID, newUUID())
		s.send(agentChunkEv(id, r.Result))
	}

	// A compaction the turn leaves open ended with it; the usage is the turn's.
	for _, ev := range s.tr.compactionCancel() {
		s.send(ev)
	}
	if ev, ok := s.tr.usage.result(&r); ok {
		s.send(ev)
	}

	// A permission request that is still open can no longer be answered.
	for id := range s.pending {
		delete(s.pending, id)
	}
	s.syncBlockedChildren()
	if !s.foreground {
		// A turn that sent nothing before its result: open and close it, so
		// the client still sees the work that ended.
		s.turn(backend.TurnRunning, "", nil)
	}
	s.foreground = false
	s.cancelling = false
	p.busy.Store(false)
	p.touch()
	s.turn(backend.TurnIdle, reason, err)
}

// procExited handles the end of a process's stdout. If a turn was open, it
// cannot finish any more: report how it ended so the client stops waiting.
// The session itself survives: the next Send respawns the process with --resume.
func (s *Session) procExited(p *proc, scanErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The process's background work ended with it, after the turn's own end.
	defer func() {
		if cur := s.rt.Get(s.uuid); cur != nil && cur != p {
			return // a newer process runs the session's work now
		}
		for _, ev := range s.tr.processEnded() {
			s.send(ev)
		}
		s.afterLine()
	}()
	for id, pp := range s.pending {
		if pp.p == p {
			delete(s.pending, id)
		}
	}
	p.busy.Store(false)
	if !s.foreground {
		return
	}
	s.foreground = false
	cancelled := s.cancelling
	s.cancelling = false
	if cancelled {
		s.turn(backend.TurnIdle, acp2.StopReasonCancelled, nil)
		return
	}
	msg := "Claude Code stopped. Send another prompt to resume the session."
	if tail := strings.TrimSpace(p.stderr.String()); tail != "" {
		msg += " (" + lastLine(tail) + ")"
	} else if scanErr != nil {
		msg += " (" + scanErr.Error() + ")"
	}
	s.turn(backend.TurnIdle, "", fmt.Errorf("%s", msg))
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// newUUID returns a random RFC 4122 version-4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's random source failing is not recoverable
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
