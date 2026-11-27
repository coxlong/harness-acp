package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// The replay of a session from its transcript, with its subagents. Claude Code
// keeps each subagent's history in its own transcript,
// <session>/subagents/agent-<id>.jsonl, and a meta file beside it names the
// Agent call that launched it. As live, the replay announces each child right
// after that call, under its agent id, sends the child's history to the child
// session, and does not show the launch call: its prompt is the parent's
// outgoing message and the child's incoming one, and a foreground child's
// answer its reply. Follows claude-agent-acp's v2/subagent-replay.ts.

// transcriptLine is one JSONL line. Only user and assistant lines are
// messages; the file also holds queue-operation, attachment, api-request-blob
// and other bookkeeping lines, some of which carry a `message` that looks
// like one.
type transcriptLine struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	Cwd              string          `json:"cwd"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
}

// readTranscript reads the messages of a transcript. A session's own transcript
// leaves out its sidechains; a subagent's is all sidechain.
func readTranscript(path string, sidechain bool) ([]transcriptLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []transcriptLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var l transcriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if (l.IsSidechain && !sidechain) || (l.IsMeta && !l.IsCompactSummary) || (l.Type != "user" && l.Type != "assistant") {
			continue
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// subMeta is what the meta file of a subagent says about it.
type subMeta struct {
	agentID, name, agentType, description string
	background, stopped                   bool
	cwd                                   string // where Claude Code placed it, when not with its parent
}

var metaNameRe = regexp.MustCompile(`^agent-(.+)\.meta\.json$`)

// loadSubagents reads the meta files of a session's subagents, by the tool use
// that launched each.
func loadSubagents(transcript, session string) map[string]subMeta {
	dir := filepath.Join(filepath.Dir(transcript), session, "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]subMeta{}
	for _, e := range entries {
		m := metaNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var meta struct {
			ToolUseID           string `json:"toolUseId"`
			Name                string `json:"name"`
			AgentType           string `json:"agentType"`
			Description         string `json:"description"`
			RequestShape        string `json:"requestShape"`
			StoppedByUser       bool   `json:"stoppedByUser"`
			Cwd                 string `json:"cwd"`
			WorktreePath        string `json:"worktreePath"`
			InheritedWorktreeAt string `json:"inheritedWorktreePath"`
		}
		if json.Unmarshal(raw, &meta) != nil || meta.ToolUseID == "" {
			continue
		}
		sm := subMeta{
			agentID: m[1], name: meta.Name, agentType: meta.AgentType, description: meta.Description,
			background: meta.RequestShape == "background", stopped: meta.StoppedByUser,
		}
		for _, p := range []string{meta.Cwd, meta.WorktreePath, meta.InheritedWorktreeAt} {
			if filepath.IsAbs(p) {
				sm.cwd = firstNonEmpty(recordedCwd(filepath.Join(dir, "agent-"+m[1]+".jsonl")), p)
				break
			}
		}
		out[meta.ToolUseID] = sm
	}
	return out
}

// recordedCwd is the working directory the first record of a transcript names.
func recordedCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var l struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && filepath.IsAbs(l.Cwd) {
			return l.Cwd
		}
	}
	return ""
}

// sameDirectory reports whether two paths name one directory, also through a symlink.
func sameDirectory(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// replayChild is a subagent the replay shows.
type replayChild struct {
	launch, id, parent string // the launching tool use, its session, the session that launched it
	meta               subMeta
	input              map[string]any
	announced          bool
	promptSent         bool
	handedBack         bool   // it reported to its parent with a SubagentHandback call
	launchEnd          string // how a foreground launch's result ended it: completed, failed, cancelled
	notified           string // the last persisted task notification's state
	cwd                string // what the replay reported, when it is not its parent's
}

// sendCall is a SendMessage call of the transcript.
type sendCall struct {
	use, to, message, sender string
	recipient                string // the child the replay delivered the message to
	shown                    bool   // the call was reached before the delivery: it stays a tool call
	outgoingSent             bool
}

type handbackCall struct {
	message      string
	failed, sent bool
}

// replayer builds a session's replay.
type replayer struct {
	root, cwd  string
	metas      map[string]subMeta
	out        []backend.HistoryUpdate
	byLaunch   map[string]*replayChild
	byID       map[string]*replayChild
	announced  []*replayChild
	omitted    map[string]bool // launches with no meta: left out
	missing    int             // announced children with no transcript
	sends      []*sendCall
	sendByUse  map[string]*sendCall
	handbacks  map[string]*handbackCall
	targets    map[string]string // the agent a TaskStop or SendMessage call names, by tool use
	transcript string
}

// planCall is a plan tool call whose result says how the plan changed.
type planCall struct {
	name  string
	input json.RawMessage
}

func newReplayer(root, transcript, cwd string) *replayer {
	return &replayer{
		root: root, cwd: cwd, transcript: transcript, metas: loadSubagents(transcript, root),
		byLaunch: map[string]*replayChild{}, byID: map[string]*replayChild{}, omitted: map[string]bool{},
		sendByUse: map[string]*sendCall{}, handbacks: map[string]*handbackCall{}, targets: map[string]string{},
	}
}

// emit adds an update of the session `session`.
func (r *replayer) emit(session string, u acp2.SessionUpdate) {
	if session == r.root {
		session = ""
	}
	r.out = append(r.out, backend.HistoryUpdate{Session: session, Update: u})
}

func emitVariant[T schema.SessionUpdateVariants](r *replayer, session string, v T) {
	r.emit(session, schema.NewSessionUpdate(v))
}

// History replays a session from its transcript as entity-level updates, as
// the live session sent them. Claude writes each content block of an API
// message as its own line, all with the same message id, so consecutive
// assistant lines of one id are merged into one message.
func (b *Backend) History(ctx context.Context, id string) (backend.History, error) {
	path, err := findTranscript(id)
	if err != nil {
		return backend.History{}, err
	}
	lines, err := readTranscript(path, false)
	if err != nil {
		return backend.History{}, err
	}
	cwd := ""
	for _, l := range lines {
		if l.Cwd != "" {
			cwd = l.Cwd
			break
		}
	}
	r := newReplayer(id, path, cwd)
	r.scan(lines, id)
	h := r.newHistory(id, nil)
	h.run(lines)

	res := backend.History{Updates: r.out, Missing: len(r.omitted) + r.missing}
	for _, c := range r.announced {
		parent := c.parent
		if parent == r.root {
			parent = ""
		}
		res.Children = append(res.Children, backend.ReplayedChild{ID: c.id, Parent: parent, End: c.end()})
	}
	return res, nil
}

// end is how the child's work ended: the state of its last task notification,
// else cancelled for a child the user stopped, else its foreground launch's
// result, else nothing is known.
func (c *replayChild) end() backend.TurnEvent {
	st := firstNonEmpty(c.notified, map[bool]string{true: "cancelled"}[c.meta.stopped], c.launchEnd)
	ev := backend.TurnEvent{State: backend.TurnIdle}
	switch st {
	case "completed":
		ev.StopReason = acp2.StopReasonEndTurn
	case "cancelled":
		ev.StopReason = acp2.StopReasonCancelled
	case "failed":
		ev.Err = errSubagentFailed
	}
	return ev
}

// --- scanning ---------------------------------------------------------------

// targetAgent is the agent a TaskStop or SendMessage call names: an agent's id
// is its child session's id.
func targetAgent(name string, input json.RawMessage) string {
	var in struct {
		TaskID string `json:"task_id"`
		To     string `json:"to"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "TaskStop":
		return strings.TrimSpace(in.TaskID)
	case "SendMessage":
		return strings.TrimSpace(in.To)
	}
	return ""
}

const handbackTool = "SubagentHandback"

func blocksOf(l *transcriptLine) []block {
	var m message
	var blocks []block
	if json.Unmarshal(l.Message, &m) == nil {
		_ = json.Unmarshal(m.Content, &blocks)
	}
	return blocks
}

// scan registers the launches, their results, and the SendMessage and
// SubagentHandback calls of a transcript before it is replayed: what the
// replay of a call needs to know of the calls around it.
func (r *replayer) scan(lines []transcriptLine, owner string) {
	for i := range lines {
		for _, b := range blocksOf(&lines[i]) {
			switch b.Type {
			case "tool_use":
				if a := targetAgent(b.Name, b.Input); a != "" {
					r.targets[b.ID] = a
				}
				var input map[string]any
				_ = json.Unmarshal(b.Input, &input)
				switch {
				case isAgentTool(b.Name):
					r.registerLaunch(b.ID, input, owner)
				case b.Name == handbackTool:
					if msg, _ := input["message"].(string); strings.TrimSpace(msg) != "" {
						r.handbacks[b.ID] = &handbackCall{message: msg}
					}
				case b.Name == "SendMessage":
					to, _ := input["to"].(string)
					msg, ok := input["message"].(string)
					if ok && to != "" {
						c := &sendCall{use: b.ID, to: to, message: msg, sender: owner}
						r.sends = append(r.sends, c)
						r.sendByUse[b.ID] = c
					}
				}
			case "tool_result":
				if hb := r.handbacks[b.ToolUseID]; hb != nil && b.IsError {
					hb.failed = true
				}
				// A background launch answers at once: its result is not the end.
				if c := r.byLaunch[b.ToolUseID]; c != nil && !c.meta.background {
					c.launchEnd = launchState(b)
				}
			}
		}
	}
}

func (r *replayer) registerLaunch(use string, input map[string]any, parent string) {
	if r.byLaunch[use] != nil {
		return
	}
	meta, ok := r.metas[use]
	if !ok {
		r.omitted[use] = true
		return
	}
	c := &replayChild{launch: use, id: meta.agentID, parent: parent, meta: meta, input: input}
	r.byLaunch[use] = c
	r.byID[c.id] = c
}

var stoppedWordRe = regexp.MustCompile(`(?i)\b(?:cancelled|canceled|interrupted|stopped|killed)\b`)

// launchState is the end of a foreground launch from its tool result: an error
// that names a stop is a cancellation, any other error a failure.
func launchState(b block) string {
	if !b.IsError {
		return "completed"
	}
	if stoppedWordRe.MatchString(resultText(b.Content)) {
		return "cancelled"
	}
	return "failed"
}

// --- messages between sessions ----------------------------------------------

// linked is a message that goes from one session to another, as its two views:
// both use the same id, and each names the other.
func linked(use, sender, recipient, text string) (outgoing, incoming schema.SessionUpdateSessionMessage) {
	content := optional.Of([]acp2.ContentBlock{acp2.TextBlock(text)})
	base := schema.SessionUpdateSessionMessage{
		MessageID:          schema.MessageID(use),
		SenderSessionID:    optional.Of(schema.SessionID(sender)),
		RecipientSessionID: optional.Of(schema.SessionID(recipient)),
		Content:            content,
	}
	outgoing, incoming = base, base
	outgoing.RecipientMessageID = optional.Of(schema.MessageID(use))
	incoming.SenderMessageID = optional.Of(schema.MessageID(use))
	return outgoing, incoming
}

func (r *replayer) sendPrompt(c *replayChild, text string) {
	c.promptSent = true
	out, in := linked(c.launch, c.parent, c.id, text)
	emitVariant(r, c.parent, out)
	emitVariant(r, c.id, in)
}

// deliveredCall is the SendMessage call a delivered message came from, for the
// child that names go by: the first undelivered call to it whose message the
// text contains. Claude Code frames the message for the child's model, so the
// text holds it rather than equals it.
func (r *replayer) deliveredCall(names []string, text string) *sendCall {
	for _, c := range r.sends {
		if c.recipient != "" || strings.TrimSpace(c.message) == "" || !strings.Contains(text, strings.TrimSpace(c.message)) {
			continue
		}
		for _, n := range names {
			if n != "" && n == c.to {
				return c
			}
		}
	}
	return nil
}

func (r *replayer) sendDelivery(c *replayChild, l *transcriptLine, text string) {
	names := []string{c.id, c.meta.name}
	if n, ok := c.input["name"].(string); ok {
		names = append(names, n)
	}
	call := r.deliveredCall(names, text)
	msg := schema.SessionUpdateSessionMessage{
		MessageID:          schema.MessageID(l.UUID),
		RecipientSessionID: optional.Of(schema.SessionID(c.id)),
		Content:            optional.Of([]acp2.ContentBlock{acp2.TextBlock(text)}),
	}
	if call != nil {
		call.recipient = c.id
		msg.MessageID = schema.MessageID(call.use)
		msg.SenderSessionID = optional.Of(schema.SessionID(call.sender))
		msg.Content = optional.Of([]acp2.ContentBlock{acp2.TextBlock(call.message)})
		if !call.shown {
			msg.SenderMessageID = optional.Of(schema.MessageID(call.use))
		}
	}
	emitVariant(r, c.id, msg)
}

// task notifications Claude Code persists as user messages when a task ends.
var (
	notificationRe = regexp.MustCompile(`(?s)<task-notification>(.*?)</task-notification>`)
	taskIDTagRe    = regexp.MustCompile(`<task-id>\s*(.*?)\s*</task-id>`)
	statusTagRe    = regexp.MustCompile(`<status>\s*(.*?)\s*</status>`)
)

// taskNotifications are the task ends a user message reports: a task id and a
// status each. The result body is model context, so the scan stops at it. One
// without a status is a task still running (a quiet shell, a Monitor event).
func taskNotifications(text string) map[string]string {
	out := map[string]string{}
	for _, m := range notificationRe.FindAllStringSubmatch(text, -1) {
		body, _, _ := strings.Cut(m[1], "<result>")
		id, st := taskIDTagRe.FindStringSubmatch(body), statusTagRe.FindStringSubmatch(body)
		if id != nil && st != nil && id[1] != "" && st[1] != "" {
			out[id[1]] = st[1]
		}
	}
	return out
}

func notifiedState(status string) string {
	switch status {
	case "completed", "failed":
		return status
	case "killed", "cancelled", "stopped":
		return "cancelled"
	}
	return ""
}

// --- announcing a child -----------------------------------------------------

// labels are the parent's labels for a child: the title is its name, else the
// task's short description, else its role; the description is the role unless
// that already is the title.
func labels(name, role, description string) (title, desc string) {
	name, role, description = strings.TrimSpace(name), strings.TrimSpace(role), strings.TrimSpace(description)
	title = firstNonEmpty(name, description, role)
	if role != "" && role != title {
		desc = role
	}
	return title, desc
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// announce shows a launched subagent: the parent learns of it, its history is
// replayed into its session, and its prompt goes from the parent to it.
func (r *replayer) announce(c *replayChild) {
	c.announced = true
	r.announced = append(r.announced, c)
	title, desc := labels(firstNonEmpty(c.meta.name, str(c.input, "name")),
		firstNonEmpty(c.meta.agentType, str(c.input, "subagent_type")),
		firstNonEmpty(c.meta.description, str(c.input, "description")))
	sub := schema.SessionUpdateSubagentUpdate{
		SessionID: schema.SessionID(c.id),
		State:     optional.Of(schema.NewStateUpdate(schema.StateUpdateRunning{})),
	}
	if title != "" {
		sub.Title = optional.Of(title)
	}
	if desc != "" {
		sub.Description = optional.Of(desc)
	}
	if c.meta.cwd != "" && !sameDirectory(c.meta.cwd, r.cwdOf(c.parent)) {
		c.cwd = c.meta.cwd
		sub.Cwd = optional.Of(schema.AbsolutePath(c.cwd))
	}
	emitVariant(r, c.parent, sub)

	var lines []transcriptLine
	if r.transcript != "" {
		lines, _ = readTranscript(filepath.Join(filepath.Dir(r.transcript), r.root, "subagents", "agent-"+c.id+".jsonl"), true)
	}
	if len(lines) == 0 {
		r.missing++
	}
	r.scan(lines, c.id)
	r.newHistory(c.id, c).run(lines)
	if text, _ := c.input["prompt"].(string); !c.promptSent && text != "" {
		r.sendPrompt(c, text)
	}
}

// cwdOf is the working directory of a session as the client holds it: a
// child's own, else its parent's, up to the replayed session's.
func (r *replayer) cwdOf(session string) string {
	for c := r.byID[session]; c != nil; c = r.byID[c.parent] {
		if c.cwd != "" {
			return c.cwd
		}
	}
	return r.cwd
}

// withTarget adds the reference a TaskStop or SendMessage call carries to the
// child session it names, when that child is another session the client knows.
func hasTitle(u schema.SessionUpdateToolCallUpdate) bool {
	_, ok := u.Title.Get()
	return ok
}

// errSubagentFailed is how a subagent that failed ended, when the history says no more.
var errSubagentFailed = errors.New("The subagent failed.")

func (r *replayer) withTarget(u schema.SessionUpdateToolCallUpdate, shownOn string) schema.SessionUpdateToolCallUpdate {
	c := r.byID[r.targets[string(u.ToolCallID)]]
	if c == nil || !c.announced || c.id == shownOn {
		return u
	}
	content, has := u.Content.Get()
	if !has && !hasTitle(u) {
		return u // an update that carries no content keeps what the client holds
	}
	for _, x := range content {
		if s, ok := x.As[schema.ToolCallContentSession](); ok && string(s.SessionID) == c.id {
			return u
		}
	}
	u.Content = optional.Of(append(content, schema.NewToolCallContent(schema.ToolCallContentSession{SessionID: schema.SessionID(c.id)})))
	return u
}

// --- replaying one transcript -----------------------------------------------

type history struct {
	r       *replayer
	session string       // the session this transcript is of
	owner   *replayChild // nil for the replayed session itself
	open    map[string]bool

	// The plan this session showed. A plan belongs to one session, so a
	// subagent's tasks never end up in its parent's checklist.
	tasks taskList
	calls map[string]planCall // plan tool calls awaiting their result
	plan  []schema.PlanEntry
	seen  bool

	// the assistant message being merged
	curID    string
	text     []acp2.ContentBlock
	thoughts []acp2.ContentBlock
}

func (r *replayer) newHistory(session string, owner *replayChild) *history {
	return &history{
		r: r, session: session, owner: owner, open: map[string]bool{},
		calls: map[string]planCall{},
	}
}

func (h *history) emit(u acp2.SessionUpdate) { h.r.emit(h.session, u) }

func (h *history) run(lines []transcriptLine) {
	for i := range lines {
		l := &lines[i]
		if l.Type == "user" {
			h.user(l)
		} else {
			h.assistant(l)
		}
	}
	h.flush()
	// A call the transcript gives no result (it ran when Claude Code was
	// killed) ends failed, as v2 asks of replayed work that no longer runs.
	for toolID := range h.open {
		h.emit(schema.NewSessionUpdate(schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(toolID),
			Status:     optional.Of(schema.ToolCallStatusFailed),
		}))
	}
	h.emitPlan()
}

// planCall records a plan tool call. TodoWrite carries the whole list, so its
// result adds nothing; the Task* family reports the change in its result.
func (h *history) planCall(name, use string, input json.RawMessage) {
	if name == "TodoWrite" {
		h.plan = todoWritePlan(input)
		h.seen = true
		return
	}
	h.calls[use] = planCall{name: name, input: input}
}

// planResult folds a Task* call's result into the task list.
func (h *history) planResult(pc planCall, b block) {
	if b.IsError {
		return
	}
	if !applyTaskResult(&h.tasks, pc.name, pc.input, nil, resultText(b.Content)) {
		return
	}
	h.plan = h.tasks.planEntries()
	h.seen = true
}

// emitPlan replays the plan the session ended with, so a resumed session shows
// the checklist it had instead of losing it. Only the final state goes out: a
// client replaces the whole plan on every update, and the protocol allows
// replaying an entity's current state instead of each of its updates.
func (h *history) emitPlan() {
	if !h.seen {
		return
	}
	h.emit(schema.NewSessionUpdate(planUpdateOf(h.plan)))
}

// flush emits the merged assistant message, if any.
func (h *history) flush() {
	if h.curID == "" {
		return
	}
	if len(h.thoughts) > 0 {
		h.emit(schema.NewSessionUpdate(schema.SessionUpdateAgentThought{
			MessageID: acp2.MessageID(h.curID + "-thought"),
			Content:   optional.Of(h.thoughts),
		}))
	}
	if len(h.text) > 0 {
		h.emit(schema.NewSessionUpdate(schema.SessionUpdateAgentMessage{
			MessageID: acp2.MessageID(h.curID),
			Content:   optional.Of(h.text),
		}))
	}
	h.curID, h.text, h.thoughts = "", nil, nil
}

// flushKeepID emits the text gathered so far but keeps merging into the same
// message id: a later line of the same API message extends it.
func (h *history) flushKeepID() {
	id := h.curID
	h.flush()
	h.curID = id
}

func (h *history) assistant(l *transcriptLine) {
	var m message
	if json.Unmarshal(l.Message, &m) != nil || m.Model == "<synthetic>" {
		return
	}
	id := firstNonEmpty(m.ID, l.UUID)
	if id != h.curID {
		h.flush()
		h.curID = id
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				h.text = append(h.text, acp2.TextBlock(b.Text))
			}
		case "thinking":
			if b.Thinking != "" {
				h.thoughts = append(h.thoughts, acp2.TextBlock(b.Thinking))
			}
		case "tool_use", "server_tool_use":
			// The message's text goes out before the tool call it leads to.
			h.flushKeepID()
			if c := h.r.byLaunch[b.ID]; c != nil && !c.announced {
				// A launch shows as the child it started, not as a tool call.
				h.r.announce(c)
				continue
			}
			if planTool(b.Name) {
				// A plan tool makes the plan, not a tool card: the live session
				// reports it as plan_update, and so does the replay.
				h.planCall(b.Name, b.ID, b.Input)
				continue
			}
			var input map[string]any
			_ = json.Unmarshal(b.Input, &input)
			for _, ev := range toolCall(b.ID, toolTitle(b.Name, input), toolKind(b.Name),
				schema.ToolCallStatusInProgress, b.Input) {
				if u, ok := ev.Update.Variant().(schema.SessionUpdateToolCallUpdate); ok {
					h.tool(u)
				}
			}
			h.open[b.ID] = true
		}
	}
}

// tool emits an update of a tool call, or what stands for it: a SendMessage
// call whose message the replay delivered to a child is that message, and a
// child's successful SubagentHandback call is its message to its parent.
func (h *history) tool(u schema.SessionUpdateToolCallUpdate) {
	r := h.r
	id := string(u.ToolCallID)
	if s := r.sendByUse[id]; s != nil {
		if s.recipient == "" {
			s.shown = true
			h.emit(schema.NewSessionUpdate(r.withTarget(u, h.session)))
			return
		}
		if s.outgoingSent {
			return
		}
		s.outgoingSent = true
		out, _ := linked(s.use, s.sender, s.recipient, s.message)
		emitVariant(r, h.session, out)
		return
	}
	if hb := r.handbacks[id]; h.owner != nil && hb != nil && !hb.failed {
		if hb.sent {
			return
		}
		hb.sent = true
		h.owner.handedBack = true
		out, in := linked(id, h.owner.id, h.owner.parent, hb.message)
		emitVariant(r, h.owner.id, out)
		emitVariant(r, h.owner.parent, in)
		return
	}
	h.emit(schema.NewSessionUpdate(r.withTarget(u, h.session)))
}

func (h *history) user(l *transcriptLine) {
	h.flush()
	if l.IsCompactSummary {
		if h.owner != nil {
			return
		}
		u := schema.SessionUpdateCompactionUpdate{
			CompactionID: schema.CompactionID(firstNonEmpty(l.UUID, newUUID())),
			Status:       schema.CompactionStatusCompleted,
		}
		if summary := compactionSummaryText(userText(l.Message)); summary != "" {
			u.Summary = optional.Of([]acp2.ContentBlock{acp2.TextBlock(summary)})
		}
		h.emit(schema.NewSessionUpdate(u))
		return
	}
	var m message
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	r := h.r
	// A plain string, or blocks: text is the user's prompt, tool_result is a
	// tool's output.
	if text := userText(l.Message); text != "" {
		for id, st := range taskNotifications(text) {
			if c := r.byID[id]; c != nil {
				if s := notifiedState(st); s != "" {
					c.notified = s
				}
			}
		}
		switch {
		case h.owner != nil:
			// A subagent has no user: the first text is the launch's prompt, a
			// message a SendMessage call delivered is another, and any other
			// text is Claude Code's own, which the live child session does not show.
			if strings.TrimSpace(text) != "" {
				if l.Origin != nil && l.Origin.Kind == "coordinator" {
					r.sendDelivery(h.owner, l, text)
				} else if !h.owner.promptSent {
					r.sendPrompt(h.owner, text)
				}
			}
		case !hiddenUserText(text):
			h.emit(schema.NewSessionUpdate(schema.SessionUpdateUserMessage{
				MessageID: acp2.MessageID(l.UUID),
				Content:   optional.Of([]acp2.ContentBlock{acp2.TextBlock(text)}),
			}))
		}
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		if pc, ok := h.calls[b.ToolUseID]; ok {
			delete(h.calls, b.ToolUseID)
			h.planResult(pc, b)
			continue
		}
		if c := r.byLaunch[b.ToolUseID]; c != nil && c.announced {
			// A foreground child's answer is its reply to the parent; a child
			// that handed back its report has none here, and a background
			// launch only acknowledges.
			if !b.IsError && !c.meta.background && !c.handedBack {
				if text := agentReport(b.Content, nil); text != "" {
					emitVariant(r, c.parent, schema.SessionUpdateSessionMessage{
						MessageID:          schema.MessageID(c.launch + ":reply"),
						SenderSessionID:    optional.Of(schema.SessionID(c.id)),
						RecipientSessionID: optional.Of(schema.SessionID(c.parent)),
						Content:            optional.Of([]acp2.ContentBlock{acp2.TextBlock(text)}),
					})
				}
			}
			continue
		}
		status := schema.ToolCallStatusCompleted
		if b.IsError {
			status = schema.ToolCallStatusFailed
		}
		u := schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(b.ToolUseID),
			Status:     optional.Of(status),
		}
		if text := resultText(b.Content); text != "" {
			u.Content = optional.Of([]acp2.ToolCallContent{acp2.ToolText(text)})
		}
		h.tool(u)
		delete(h.open, b.ToolUseID)
	}
}
