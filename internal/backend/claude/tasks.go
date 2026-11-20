package claude

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Background work and subagents. Claude Code reports both as "tasks": a
// task_started names the task, background_tasks_changed lists the ones running,
// and task_updated / task_notification end it. A background Bash becomes a
// terminal, a subagent a child session. Follows claude-agent-acp's
// v2/tasks.ts, terminal.ts and native-subagents.ts.

// taskInfo is one task Claude Code started.
type taskInfo struct {
	id, toolUse string
	description string
	kind        string // local_bash, local_agent, ...
	terminal    bool   // a terminal shows it (a Bash)
	background  bool
	ended       bool
	child       *child // a subagent
	runUse      string // the call that started the subagent's current run
}

// child is a subagent's session. Its output is translated by its own
// translator, so its tool calls and message ids stay apart from the parent's.
type child struct {
	id, toolUse string
	tr          *translator
	background  bool
	ended       bool   // its run ended
	blocked     bool   // a permission request of its own is open
	stateKey    string // the state last reported, to repeat it only when it changes
}

// tailJob asks the session to follow a background command's output file.
type tailJob struct{ terminal, path, session string }

// stopJob ends a terminal: its tail flushes, then the exit goes out.
type stopJob struct {
	terminal    string
	session     string // the child session the terminal is in; "" for the session itself
	cancellable bool   // the terminal advertised cancel, which its exit withdraws
	exit        schema.TerminalExitStatus
}

const maxOrphans = 256 // child lines kept while the child is not announced yet

func isBash(name string) bool                     { return name == "Bash" }
func isAgentTool(name string) bool                { return name == "Agent" || name == "Task" }
func terminalID(toolUse string) schema.TerminalID { return schema.TerminalID(toolUse) }

// --- terminals --------------------------------------------------------------

// encodeTerminal makes terminal bytes: base64 of the text, with line feeds as
// the carriage return + line feed a terminal expects.
func encodeTerminal(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func terminalUpdate(u schema.SessionUpdateTerminalUpdate) backend.Event { return update(u) }

// bashStart announces a Bash tool call's terminal and returns the events that
// open it. The terminal update goes before the tool call that shows it.
func (t *translator) bashStart(id string) []backend.Event {
	if t.terminals[id] {
		return nil
	}
	t.terminals[id] = true
	return one(terminalUpdate(schema.SessionUpdateTerminalUpdate{TerminalID: terminalID(id)}))
}

func terminalContent(id string) []acp2.ToolCallContent {
	return []acp2.ToolCallContent{acp2.ToolTerminal(terminalID(id))}
}

var exitCodeRe = regexp.MustCompile(`^Exit code (-?\d+)`)

// bashResult reports a finished Bash: a foreground command's output and exit, or
// a background command's launch (which keeps its terminal running).
func (t *translator) bashResult(b block, l *line, text string) []backend.Event {
	id := b.ToolUseID
	var out []backend.Event
	var res struct {
		BackgroundTaskID string `json:"backgroundTaskId"`
		Interrupted      bool   `json:"interrupted"`
	}
	if len(l.ToolUseResult) > 0 && l.ToolUseResult[0] == '{' {
		_ = json.Unmarshal(l.ToolUseResult, &res)
	}
	status := schema.ToolCallStatusCompleted
	if b.IsError {
		status = schema.ToolCallStatusFailed
	}
	if res.BackgroundTaskID != "" {
		// The command goes on: no exit. Its output replaces the launch notice.
		out = append(out, terminalUpdate(schema.SessionUpdateTerminalUpdate{
			TerminalID: terminalID(id), Output: optional.Of(schema.TerminalOutput{Data: encodeTerminal(text)}),
		}))
		if ti := t.tasks[res.BackgroundTaskID]; ti != nil {
			ti.terminal = true
		} else {
			ti := &taskInfo{id: res.BackgroundTaskID, toolUse: id, kind: "local_bash", terminal: true, background: true}
			t.tasks[ti.id] = ti
			t.byUse[id] = ti
		}
		if m := outputPathRe.FindStringSubmatch(text); m != nil {
			t.tails = append(t.tails, tailJob{terminal: id, path: m[1]})
		}
	} else {
		out = append(out, terminalUpdate(schema.SessionUpdateTerminalUpdate{
			TerminalID: terminalID(id), Output: optional.Of(schema.TerminalOutput{Data: encodeTerminal(text)}),
		}))
		exit := schema.TerminalExitStatus{}
		switch m := exitCodeRe.FindStringSubmatch(text); {
		case res.Interrupted:
		case m != nil:
			n, _ := strconv.ParseInt(m[1], 10, 64)
			exit.ExitCode = &n
		case !b.IsError:
			zero := int64(0)
			exit.ExitCode = &zero
		}
		out = append(out, terminalUpdate(schema.SessionUpdateTerminalUpdate{TerminalID: terminalID(id), ExitStatus: optional.Of(exit)}))
	}
	out = append(out, update(schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(id), Status: optional.Of(status),
	}))
	return out
}

var outputPathRe = regexp.MustCompile(`Output is being written to: (\S+?\.output)`)

// --- task messages ----------------------------------------------------------

var exitSummaryRe = regexp.MustCompile(`\(exit code (-?\d+)\)`)

// task translates the task_* system messages.
func (t *translator) task(l *line) []backend.Event {
	// Work a subagent started (its own background command) belongs to the
	// subagent's session: its translator keeps the terminal and its tail.
	if c := t.taskOwner(l); c != nil {
		var out []backend.Event
		for _, ev := range c.tr.task(l) {
			if ev.Session == "" {
				ev.Session = c.id
			}
			out = append(out, ev)
		}
		return out
	}
	switch l.Subtype {
	case "task_started":
		// A subagent that ran before runs again, under the call that resumed it.
		if old := t.tasks[l.TaskID]; old != nil && old.child != nil && (l.TaskType == "local_agent" || l.SubagentType != "") {
			old.background = l.IsBackgrounded
			old.child.background = l.IsBackgrounded
			return t.resume(old, l.ToolUseID, l.Prompt)
		}
		ti := &taskInfo{id: l.TaskID, toolUse: l.ToolUseID, kind: l.TaskType, description: l.Description, background: l.IsBackgrounded}
		if old := t.tasks[ti.id]; old != nil {
			ti.terminal = old.terminal
		}
		t.tasks[ti.id] = ti
		if ti.toolUse != "" {
			t.byUse[ti.toolUse] = ti
		}
		if l.TaskType == "local_agent" || l.SubagentType != "" {
			return t.announce(ti, l)
		}
	case "task_progress":
		return t.taskProgress(l)
	case "task_updated":
		switch l.Patch.Status {
		case "paused", "running", "pending":
			return t.callPaused(l.TaskID, l.Patch.Status == "paused")
		case "completed", "failed", "killed", "stopped":
			// A command's task_notification follows with its exit code, which
			// the end waits for; a subagent ends at once.
			if ti := t.tasks[l.TaskID]; ti != nil && ti.terminal {
				return nil
			}
			return t.taskEnded(l.TaskID, l.Patch.Status, "", l.Patch.Error)
		}
	case "task_notification":
		// The end of an earlier run of a subagent that runs again ends nothing.
		if ti := t.tasks[l.TaskID]; ti != nil && ti.child != nil && l.ToolUseID != "" && ti.runUse != "" &&
			l.ToolUseID != ti.runUse && t.byUse[l.ToolUseID] == ti {
			return nil
		}
		return t.taskEnded(l.TaskID, statusOf(l), l.Summary, "")
	case "background_tasks_changed":
		t.bgSeen = true
		prev := t.bg
		t.bg = nil
		listed := map[string]bool{}
		t.level = map[string]levelTask{}
		for _, k := range l.Tasks {
			t.bg = append(t.bg, k.TaskID)
			listed[k.TaskID] = true
			t.level[k.TaskID] = levelTask{ambient: k.Ambient, kind: k.TaskType, description: k.Description, parent: k.ParentID}
		}
		// A command that left the list without a notification has ended: its
		// terminal must not stay open.
		var out []backend.Event
		for _, id := range prev {
			if ti := t.tasks[id]; ti != nil && !listed[id] && (ti.terminal || t.calls[id] != nil) {
				out = append(out, t.taskEnded(id, "completed", "", "")...)
			}
		}
		return out
	}
	return nil
}

// taskOwner is the subagent a task message belongs to: the one that started
// the task (parent_task_id), or the one whose translator already knows it. nil
// for the session's own.
func (t *translator) taskOwner(l *line) *child {
	switch l.Subtype {
	case "task_started":
		if l.ParentTaskID != "" {
			if ti := t.tasks[l.ParentTaskID]; ti != nil && ti.child != nil {
				return ti.child
			}
		}
	case "task_updated", "task_notification", "task_progress":
		if t.tasks[l.TaskID] != nil {
			return nil
		}
		for _, ti := range t.tasks {
			if ti.child != nil && ti.child.tr.knows(l.TaskID) {
				return ti.child
			}
		}
	}
	return nil
}

// knows reports whether the translator, or a subagent below it, has the task.
func (t *translator) knows(id string) bool {
	if t.tasks[id] != nil {
		return true
	}
	for _, ti := range t.tasks {
		if ti.child != nil && ti.child.tr.knows(id) {
			return true
		}
	}
	return false
}

// taskEnded ends a task once: Claude Code reports an end twice (task_updated,
// then task_notification).
func (t *translator) taskEnded(id, status, summary, errText string) []backend.Event {
	ti := t.tasks[id]
	if ti == nil && t.calls[id] != nil {
		return t.callEnded(id, status)
	}
	if ti == nil || ti.ended {
		return nil
	}
	ti.ended = true
	switch {
	case t.calls[id] != nil:
		return t.callEnded(id, status)
	case ti.child != nil:
		return t.childEnded(ti.child, status, firstNonEmpty(errText, summary))
	case ti.terminal:
		exit := schema.TerminalExitStatus{}
		if m := exitSummaryRe.FindStringSubmatch(summary); m != nil {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			exit.ExitCode = &n
		}
		t.stops = append(t.stops, stopJob{terminal: ti.toolUse, cancellable: t.advertised[ti.toolUse], exit: exit})
	}
	return nil
}

// backgroundWork is the set of work running apart from the turn, or nil while
// Claude Code has reported none yet. Work that has a terminal or a child
// session is listed; anything else Claude Code runs has nothing to show.
func (t *translator) backgroundWork() []schema.BackgroundWork {
	if !t.bgSeen {
		return nil
	}
	out := []schema.BackgroundWork{}
	for _, id := range t.bg {
		ti := t.tasks[id]
		if call := t.calls[id]; call != nil {
			out = append(out, schema.NewBackgroundWork(schema.BackgroundWorkToolCall{ToolCallID: acp2.ToolCallID(call.id)}))
			continue
		}
		if lv, ok := t.levelOf(id); ok && lv.ambient {
			continue // Claude Code's housekeeping
		}
		switch {
		case ti == nil:
		case ti.child != nil:
			out = append(out, schema.NewBackgroundWork(schema.BackgroundWorkSubagent{SessionID: schema.SessionID(ti.id)}))
		case ti.terminal:
			out = append(out, schema.NewBackgroundWork(schema.BackgroundWorkTerminal{TerminalID: terminalID(ti.toolUse)}))
		}
	}
	return out
}

// --- subagents --------------------------------------------------------------

func sessionMessage(m schema.SessionUpdateSessionMessage) backend.Event { return update(m) }

// announce opens a subagent's session: the parent learns of it, the child
// starts running and holds the prompt, and what the child said before it was
// announced is delivered.
func (t *translator) announce(ti *taskInfo, l *line) []backend.Event {
	var in map[string]any
	if info := t.tools[ti.toolUse]; len(info.input) > 0 {
		_ = json.Unmarshal(info.input, &in)
	}
	title, desc := labels(str(in, "name"), l.SubagentType, l.Description)
	if title == "" {
		title = "Subagent"
	}
	c := &child{id: ti.id, toolUse: ti.toolUse, tr: newTranslator(), background: l.IsBackgrounded}
	c.tr.sessionID, c.tr.cwd, c.tr.up = ti.id, t.cwd, t
	ti.child = c
	ti.runUse = ti.toolUse

	sub := schema.SessionUpdateSubagentUpdate{
		SessionID:    schema.SessionID(ti.id),
		Title:        optional.Of(title),
		Capabilities: optional.Of(schema.SubagentSessionCapabilities{Cancel: &schema.SessionCancelCapabilities{}}),
		State:        optional.Of(schema.NewStateUpdate(schema.StateUpdateRunning{})),
	}
	if desc != "" {
		sub.Description = optional.Of(desc)
	}
	out := []backend.Event{update(sub)}
	out = append(out, childEvent(ti.id, backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{State: backend.TurnRunning}}))

	// The prompt is one message, seen from both sides.
	prompt := []acp2.ContentBlock{acp2.TextBlock(l.Prompt)}
	childMsg := schema.MessageID(ti.toolUse)
	parent := schema.SessionID(t.sessionID)
	out = append(out, sessionMessage(schema.SessionUpdateSessionMessage{
		MessageID:          schema.MessageID(ti.toolUse),
		SenderSessionID:    optional.Of(parent),
		RecipientSessionID: optional.Of(schema.SessionID(ti.id)),
		RecipientMessageID: optional.Of(childMsg),
		Content:            optional.Of(prompt),
	}))
	out = append(out, childEvent(ti.id, sessionMessage(schema.SessionUpdateSessionMessage{
		MessageID:       childMsg,
		SenderSessionID: optional.Of(parent),
		SenderMessageID: optional.Of(schema.MessageID(ti.toolUse)),
		Content:         optional.Of(prompt),
	})))

	for _, ol := range t.orphans[ti.toolUse] {
		out = append(out, c.deliver(ol)...)
	}
	delete(t.orphans, ti.toolUse)
	return out
}

func childEvent(session string, ev backend.Event) backend.Event {
	ev.Session = session
	return ev
}

// childLine routes a subagent's message to its child session, or keeps it until
// the child is announced.
func (t *translator) childLine(l *line) []backend.Event {
	use := *l.ParentToolUseID
	if c := t.childOfUse(use); c != nil {
		return c.deliver(l)
	}
	if len(t.orphans[use]) < maxOrphans {
		t.orphans[use] = append(t.orphans[use], l)
	}
	return nil
}

// deliver translates a subagent's message as the child session's own. Its token
// usage is not the conversation's, so no usage update comes of it.
func (c *child) deliver(l *line) []backend.Event {
	cp := *l
	cp.ParentToolUseID = nil
	var out []backend.Event
	for _, ev := range c.tr.content(&cp) {
		if ev.Kind == backend.EventUpdate {
			if _, usage := ev.Update.Variant().(schema.SessionUpdateUsageUpdate); usage {
				continue
			}
		}
		out = append(out, childEvent(c.id, ev))
	}
	return out
}

// childEnded ends a subagent: what it left unfinished is closed, then the parent
// and the child both learn it is idle.
func (t *translator) childEnded(c *child, status, detail string) []backend.Event {
	c.ended = true
	reason := acp2.StopReasonEndTurn
	var failure error
	toolStatus := schema.ToolCallStatusCompleted
	switch status {
	case "killed", "stopped":
		reason = acp2.StopReasonCancelled
		toolStatus = schema.ToolCallStatusFailed
	case "failed":
		reason = ""
		failure = fmt.Errorf("%s", firstNonEmpty(detail, "The subagent failed."))
		toolStatus = schema.ToolCallStatusFailed
	}
	var out []backend.Event
	for id := range c.tr.tools {
		if planTool(c.tr.tools[id].name) {
			continue
		}
		out = append(out, childEvent(c.id, update(schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(id), Status: optional.Of(toolStatus),
		})))
	}
	clear(c.tr.tools)
	out = append(out, update(schema.SessionUpdateSubagentUpdate{
		SessionID: schema.SessionID(c.id),
		State:     optional.Of(schema.NewStateUpdate(schema.StateUpdateIdle{Value: idleFor(reason, failure)})),
	}))
	out = append(out, childEvent(c.id, backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{
		State: backend.TurnIdle, StopReason: reason, Err: failure,
	}}))
	return out
}

func idleFor(reason acp2.StopReason, failure error) schema.IdleStateUpdate {
	switch {
	case failure != nil:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateError{Error: &schema.Error{
			Code: schema.ErrorCode(-32603), Message: failure.Error(),
		}})
	case reason == acp2.StopReasonCancelled:
		return schema.NewIdleStateUpdate(schema.IdleStateUpdateCancelled{})
	}
	return schema.NewIdleStateUpdate(schema.IdleStateUpdateEndTurn{})
}

// agentResult reports the result of an Agent / Task tool call. A subagent that
// was announced shows as its own session: a foreground one answers the parent
// with a message, a background one only acknowledges the launch, which says
// nothing. A call that never got a subagent shows as the tool call it is.
func (t *translator) agentResult(b block, l *line, info toolInfo) []backend.Event {
	ti := t.byUse[b.ToolUseID]
	delete(t.launches, b.ToolUseID)
	if ti != nil && ti.child != nil {
		if ti.child.background || b.IsError {
			if b.IsError {
				return t.shownAgentCall(b, info)
			}
			return nil
		}
		text := agentReport(b.Content, l.ToolUseResult)
		if text == "" {
			return nil
		}
		return one(sessionMessage(schema.SessionUpdateSessionMessage{
			MessageID:          schema.MessageID(b.ToolUseID + ":reply"),
			SenderSessionID:    optional.Of(schema.SessionID(ti.id)),
			RecipientSessionID: optional.Of(schema.SessionID(t.sessionID)),
			Content:            optional.Of([]acp2.ContentBlock{acp2.TextBlock(text)}),
		}))
	}
	return t.shownAgentCall(b, info)
}

// shownAgentCall is the tool call of an Agent call that has no child session.
func (t *translator) shownAgentCall(b block, info toolInfo) []backend.Event {
	status := schema.ToolCallStatusCompleted
	if b.IsError {
		status = schema.ToolCallStatusFailed
	}
	title := info.name
	var in struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(info.input, &in) == nil && in.Description != "" {
		title += ": " + in.Description
	}
	u := schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(b.ToolUseID),
		Title:      optional.Of(title),
		Kind:       optional.Of(info.kind),
		Status:     optional.Of(status),
	}
	if content := resultContent(b.Content, b.IsError); len(content) > 0 {
		u.Content = optional.Of(content)
	}
	return one(update(u))
}

var (
	handbackFrameRe = regexp.MustCompile(`(?s)^\[Subagent hand-back\].*?The report follows:\n`)
	handbackTailRe  = regexp.MustCompile(`(?s)\nagentId: .*$`)
)

// handbackText is a subagent's final report as Claude Code hands it to the
// parent: inside a frame that marks it as model output, indented, and followed
// by the agent's id and usage. The frame is for the model, not for the user.
func handbackText(text string) string {
	text = stripAgentTrailer(text)
	if !handbackFrameRe.MatchString(text) {
		return strings.TrimSpace(text)
	}
	text = handbackFrameRe.ReplaceAllString(text, "")
	text = handbackTailRe.ReplaceAllString(text, "")
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimPrefix(ln, "  ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

var agentIDLineRe = regexp.MustCompile(`^agentId: [\w-]+ \([^)]*\)$`)

// stripAgentTrailer removes what Claude Code appends to a subagent's report
// for the model: a <usage> totals block and an `agentId: <id> (use SendMessage
// ...)` line at the end of the text. Both are anchored to the end and
// independent, so a format change stops them matching rather than mangling the
// report.
func stripAgentTrailer(text string) string {
	body := strings.TrimRight(text, " \t\r\n")
	if strings.HasSuffix(body, "</usage>") {
		if i := strings.LastIndex(body[:len(body)-len("</usage>")], "<usage>"); i >= 0 {
			if i > 0 && body[i-1] == '\n' {
				i--
			}
			text, body = body[:i], strings.TrimRight(body[:i], " \t\r\n")
		}
	}
	lineStart := strings.LastIndexByte(body, '\n') + 1
	if agentIDLineRe.MatchString(body[lineStart:]) {
		return body[:max(lineStart-1, 0)]
	}
	return text
}

// agentReport is the text a completed subagent reported: the structured result
// carries it without the model-directed trailer, and the raw text is the
// fallback (older Claude Code, a replay).
func agentReport(content json.RawMessage, structured json.RawMessage) string {
	var res struct {
		Status  string `json:"status"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if len(structured) > 0 && structured[0] == '{' && json.Unmarshal(structured, &res) == nil && res.Status == "completed" {
		var parts []string
		for _, b := range res.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	return handbackText(resultText(content))
}

func statusOf(l *line) string {
	if l.Status == nil {
		return ""
	}
	return *l.Status
}

// childOfUse is the subagent that the tool call `use` launched, at any depth.
func (t *translator) childOfUse(use string) *child {
	if ti := t.byUse[use]; ti != nil && ti.child != nil {
		return ti.child
	}
	for _, ti := range t.tasks {
		if ti.child != nil {
			if c := ti.child.tr.childOfUse(use); c != nil {
				return c
			}
		}
	}
	return nil
}

// childByID is the subagent whose session is id, at any depth.
func (t *translator) childByID(id string) *child {
	if ti := t.tasks[id]; ti != nil && ti.child != nil {
		return ti.child
	}
	for _, ti := range t.tasks {
		if ti.child != nil {
			if c := ti.child.tr.childByID(id); c != nil {
				return c
			}
		}
	}
	return nil
}

// children are the subagents of this translator and, below them, theirs.
func (t *translator) children() []*child {
	var out []*child
	for _, ti := range t.tasks {
		if ti.child != nil {
			out = append(out, ti.child)
			out = append(out, ti.child.tr.children()...)
		}
	}
	return out
}

// takeJobs hands over the output files to follow and the terminals to end,
// those of the subagents too, each tagged with its session.
func (t *translator) takeJobs(session string) (tails []tailJob, stops []stopJob) {
	for _, j := range t.tails {
		j.session = session
		tails = append(tails, j)
	}
	for _, j := range t.stops {
		j.session = session
		stops = append(stops, j)
	}
	t.tails, t.stops = nil, nil
	for _, ti := range t.tasks {
		if ti.child != nil {
			a, b := ti.child.tr.takeJobs(ti.id)
			tails, stops = append(tails, a...), append(stops, b...)
		}
	}
	return tails, stops
}

// --- cancel and background reports -------------------------------------------

// cancelCapability marks a terminal or subagent as one the client may cancel.
func cancelCapability() optional.Value[schema.TerminalCapabilities] {
	return optional.Of(schema.TerminalCapabilities{Cancel: &schema.TerminalCancelCapabilities{}})
}

// backgroundEvents reports what a change of the background work calls for, in
// the session `session` and in its subagents': a terminal that is listed for
// the first time advertises that it can be cancelled, before the state that
// names it; a subagent whose own list changed gets its state again.
func (t *translator) backgroundEvents(session string) []backend.Event {
	var out []backend.Event
	for _, ev := range t.adoptTasks() {
		ev.Session = session
		out = append(out, ev)
	}
	for _, w := range t.backgroundWork() {
		if call, ok := w.As[schema.BackgroundWorkToolCall](); ok {
			if !t.advertised[string(call.ToolCallID)] {
				t.advertised[string(call.ToolCallID)] = true
				ev := update(schema.SessionUpdateToolCallUpdate{
					ToolCallID:   call.ToolCallID,
					Capabilities: optional.Of(schema.ToolCallCapabilities{Cancel: &schema.ToolCallCancelCapabilities{}}),
				})
				ev.Session = session
				out = append(out, ev)
			}
			continue
		}
		term, ok := w.As[schema.BackgroundWorkTerminal]()
		if !ok || t.advertised[string(term.TerminalID)] {
			continue
		}
		t.advertised[string(term.TerminalID)] = true
		ev := terminalUpdate(schema.SessionUpdateTerminalUpdate{TerminalID: term.TerminalID, Capabilities: cancelCapability()})
		ev.Session = session
		out = append(out, ev)
	}
	for _, ti := range t.tasks {
		c := ti.child
		if c == nil || ti.ended {
			continue
		}
		// The nested work the session lists is the subagent's own list.
		c.tr.bg, c.tr.bgSeen = nil, true
		for _, id := range t.bg {
			if c.tr.knows(id) {
				c.tr.bg = append(c.tr.bg, id)
			}
		}
		out = append(out, c.tr.backgroundEvents(c.id)...)
		out = append(out, t.childState(c)...)
	}
	return out
}

// childState reports a subagent's state with its background work, to its own
// session and as the parent's mirror, when that differs from the last report.
func (t *translator) childState(c *child) []backend.Event {
	state := backend.TurnRunning
	if c.blocked {
		state = backend.TurnRequiresAction
	}
	bg := c.tr.backgroundWork()
	key, _ := json.Marshal(struct {
		S  backend.TurnState
		BG []schema.BackgroundWork
	}{state, bg})
	if string(key) == c.stateKey {
		return nil
	}
	first := c.stateKey == ""
	c.stateKey = string(key)
	if first && len(bg) == 0 && state == backend.TurnRunning {
		return nil // what the announcement said
	}
	var mirror schema.StateUpdate
	if state == backend.TurnRequiresAction {
		mirror = schema.NewStateUpdate(schema.StateUpdateRequiresAction{Background: bg})
	} else {
		mirror = schema.NewStateUpdate(schema.StateUpdateRunning{Background: bg})
	}
	return []backend.Event{
		update(schema.SessionUpdateSubagentUpdate{SessionID: schema.SessionID(c.id), State: optional.Of(mirror)}),
		childEvent(c.id, backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{State: state, Background: bg}}),
	}
}

// terminalTask finds the running background command whose terminal is
// `terminal` in the session `session` ("" is the translator's own): its task id.
func (t *translator) terminalTask(session, own, terminal string) (string, bool) {
	if session == own {
		listed := map[string]bool{}
		for _, id := range t.bg {
			listed[id] = true
		}
		for id, ti := range t.tasks {
			if ti.terminal && ti.toolUse == terminal && !ti.ended && listed[id] {
				return id, true
			}
		}
		return "", false
	}
	for _, ti := range t.tasks {
		if ti.child != nil {
			if id, ok := ti.child.tr.terminalTask(session, ti.id, terminal); ok {
				return id, true
			}
		}
	}
	return "", false
}
