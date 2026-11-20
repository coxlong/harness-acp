package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Background tasks that a tool call stands for. A Monitor or Workflow call
// starts a task that outlives its result: the call stays in progress (or
// paused) until the task ends, and is listed as background work that can be
// cancelled. Any other background task (a hook's, or one that started after
// its tool call returned) gets a tool call of its own, task_<id>, as v2 lists
// background work only by an entity the client knows. Follows
// claude-agent-acp's v2/tasks.ts (toolCalls, adoptTasks, endToolCall) and
// workflow-progress.ts.

// levelTask is a task of Claude Code's live background task set.
type levelTask struct {
	ambient     bool // Claude Code's housekeeping: not shown
	kind        string
	description string
	parent      string
}

// taskCall is the tool call that stands for a background task.
type taskCall struct {
	id      string // the tool call
	paused  bool
	adopted bool   // started here for the task, not the model's
	card    string // the workflow card last sent
}

// taskProgress is a task_progress report, as far as a workflow card shows it.
type taskProgress struct {
	description, summary, lastTool string
	usage                          *taskUsage
	workflow                       *workflowSnapshot
}

type taskUsage struct {
	TotalTokens int `json:"total_tokens"`
	ToolUses    int `json:"tool_uses"`
	DurationMs  int `json:"duration_ms"`
}

// taskTool reports whether a tool starts a task its call stands for.
func taskTool(name string) bool { return name == "Monitor" || name == "Workflow" }

// levelOf finds a task in Claude Code's live set, which the root session's
// translator keeps for the subagents' too.
func (t *translator) levelOf(id string) (levelTask, bool) {
	for r := t; r != nil; r = r.up {
		if r.up == nil {
			lv, ok := r.level[id]
			return lv, ok
		}
	}
	return levelTask{}, false
}

// holdCall keeps the tool call of a finished tool use going when its result
// started a background task: held says it did, and the updates show the task
// on the call (a workflow's progress so far). one reports that the message holds
// that one result, the only case in which a structured task id is the call's.
func (t *translator) holdCall(b block, info toolInfo, l *line, one bool) (events []backend.Event, held bool) {
	if b.IsError || info.name == "" || isBash(info.name) || isAgentTool(info.name) {
		return nil, false
	}
	id := ""
	if taskTool(info.name) && one {
		var r struct {
			TaskID string `json:"taskId"`
		}
		if len(l.ToolUseResult) > 0 && l.ToolUseResult[0] == '{' && json.Unmarshal(l.ToolUseResult, &r) == nil {
			id = r.TaskID
		}
	}
	if id == "" {
		if ti := t.byUse[b.ToolUseID]; ti != nil {
			id = ti.id
		}
	}
	if id == "" {
		return nil, false
	}
	lv, ok := t.levelOf(id)
	if !ok || lv.kind == "local_agent" || t.calls[id] != nil {
		return nil, false
	}
	// Claude Code reports a Monitor as local_bash, like a command: a command
	// is its terminal, but the call of a tool that starts a task is the task.
	if lv.kind == "local_bash" && !taskTool(info.name) {
		return nil, false
	}
	if ti := t.tasks[id]; ti != nil && ti.ended {
		return nil, false
	}
	t.calls[id] = &taskCall{id: b.ToolUseID}
	// A workflow can report progress before its launch call returns.
	return t.workflowCard(id), true
}

// adoptTasks starts a tool call for each live background task that nothing
// stands for yet: a task that no tool call started, or whose call returned
// before it started. Ambient tasks, commands and subagents are not adopted.
func (t *translator) adoptTasks() []backend.Event {
	var out []backend.Event
	for _, id := range t.bg {
		ti := t.tasks[id]
		if ti == nil || ti.child != nil || ti.terminal || t.calls[id] != nil || ti.ended {
			continue
		}
		lv, _ := t.levelOf(id)
		if lv.ambient || lv.kind == "local_bash" || lv.kind == "local_agent" || ti.kind == "local_bash" || ti.kind == "local_agent" {
			continue
		}
		// The task joins the live set before its task_started and before the
		// call that starts it returns: until both, it belongs to that call.
		if ti.toolUse != "" && !t.returned[ti.toolUse] {
			continue
		}
		call := &taskCall{id: "task_" + id, adopted: true}
		t.calls[id] = call
		out = append(out, update(schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(call.id),
			Title:      optional.Of(firstNonEmpty(lv.description, ti.description, "Background task")),
			Kind:       optional.Of(acp2.ToolKindOther),
			Status:     optional.Of(schema.ToolCallStatusInProgress),
		}))
	}
	return out
}

// callPaused reports a task's call paused or running again.
func (t *translator) callPaused(id string, paused bool) []backend.Event {
	call := t.calls[id]
	if call == nil || call.paused == paused {
		return nil
	}
	call.paused = paused
	st := schema.ToolCallStatusInProgress
	if paused {
		st = schema.ToolCallStatusPaused
	}
	return one(update(schema.SessionUpdateToolCallUpdate{ToolCallID: acp2.ToolCallID(call.id), Status: optional.Of(st)}))
}

// callEnded ends the call of a task with the task's outcome, which also
// withdraws the cancel it advertised.
func (t *translator) callEnded(id, status string) []backend.Event {
	call := t.calls[id]
	if call == nil {
		return nil
	}
	delete(t.calls, id)
	st := schema.ToolCallStatusCompleted
	switch status {
	case "failed":
		st = schema.ToolCallStatusFailed
	case "killed", "stopped", "cancelled":
		st = schema.ToolCallStatusCancelled
	}
	u := schema.SessionUpdateToolCallUpdate{ToolCallID: acp2.ToolCallID(call.id), Status: optional.Of(st)}
	if t.advertised[call.id] {
		u.Capabilities = optional.Null[schema.ToolCallCapabilities]()
	}
	return one(update(u))
}

// callTask finds the live task whose call is toolCall in the session session
// ("" is the translator's own): its task id.
func (t *translator) callTask(session, own, toolCall string) (string, bool) {
	if session == own {
		for _, id := range t.bg {
			if c := t.calls[id]; c != nil && c.id == toolCall {
				return id, true
			}
		}
		return "", false
	}
	for _, ti := range t.tasks {
		if ti.child != nil {
			if id, ok := ti.child.tr.callTask(session, ti.id, toolCall); ok {
				return id, true
			}
		}
	}
	return "", false
}

// --- workflow cards ---------------------------------------------------------

// taskProgress records a task's progress. A workflow's shows on its call.
func (t *translator) taskProgress(l *line) []backend.Event {
	p := taskProgress{
		description: strings.TrimSpace(l.Description),
		summary:     strings.TrimSpace(l.Summary),
		lastTool:    strings.TrimSpace(l.LastToolName),
		usage:       l.TaskUsage,
		workflow:    parseWorkflow(l.WorkflowProgress),
	}
	// A report without a workflow snapshot keeps the one before.
	if p.workflow == nil {
		p.workflow = t.progress[l.TaskID].workflow
	}
	t.progress[l.TaskID] = p
	return t.workflowCard(l.TaskID)
}

// workflowCard shows a workflow's latest progress as the title and the body of
// its call, without changing its status. Nothing for another kind of task.
func (t *translator) workflowCard(id string) []backend.Event {
	call := t.calls[id]
	lv, _ := t.levelOf(id)
	if call == nil || lv.kind != "local_workflow" {
		return nil
	}
	title, body, ok := workflowText(t.progress[id])
	if !ok || title+"\n"+body == call.card {
		return nil
	}
	call.card = title + "\n" + body
	return one(update(schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(call.id),
		Title:      optional.Of(title),
		Content:    optional.Of([]acp2.ToolCallContent{acp2.ToolText(body)}),
	}))
}

// workflowText is the card of a workflow: its latest activity as the title, and
// as the body its summary, its last tool, its usage and its phases.
func workflowText(p taskProgress) (title, body string, ok bool) {
	if p.description == "" && p.summary == "" && p.lastTool == "" && p.usage == nil && p.workflow == nil {
		return "", "", false
	}
	title = "Workflow"
	if a := firstNonEmpty(p.description, p.summary); a != "" {
		title = "Workflow: " + a
	}
	var parts []string
	if s := firstNonEmpty(p.summary, p.description); s != "" {
		parts = append(parts, s)
	}
	if p.lastTool != "" {
		parts = append(parts, "Last activity: "+p.lastTool)
	}
	if u := p.usage; u != nil {
		parts = append(parts, fmt.Sprintf("%s tokens · %s tool calls · %ds", thousands(u.TotalTokens), thousands(u.ToolUses), u.DurationMs/1000))
	}
	if p.workflow != nil {
		parts = append(parts, p.workflow.text())
	}
	return title, strings.Join(parts, "\n\n"), true
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

type workflowPhase struct {
	index int
	title string
}

type workflowAgent struct {
	label, state, lastTool, lastSummary string
	phase                               *int
	attempt, tokens, toolCalls          *int
	durationMs                          *int
}

// workflowSnapshot is the phases and agents of a running workflow, from the
// workflow_progress Claude Code attaches to a workflow's task_progress.
type workflowSnapshot struct {
	phases []workflowPhase
	agents []workflowAgent
}

// parseWorkflow reads a workflow_progress value: entries that do not match what
// Claude Code sends are skipped, so a change drops the list rather than the card.
func parseWorkflow(raw json.RawMessage) *workflowSnapshot {
	var entries []struct {
		Type          string `json:"type"`
		Index         *int   `json:"index"`
		Title         string `json:"title"`
		Label         string `json:"label"`
		State         string `json:"state"`
		PhaseIndex    *int   `json:"phaseIndex"`
		Attempt       *int   `json:"attempt"`
		Tokens        *int   `json:"tokens"`
		ToolCalls     *int   `json:"toolCalls"`
		DurationMs    *int   `json:"durationMs"`
		LastToolName  string `json:"lastToolName"`
		LastToolSumry string `json:"lastToolSummary"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	w := &workflowSnapshot{}
	for _, e := range entries {
		switch e.Type {
		case "workflow_phase":
			if title := strings.TrimSpace(e.Title); e.Index != nil && title != "" {
				w.phases = append(w.phases, workflowPhase{*e.Index, title})
			}
		case "workflow_agent":
			label, state := strings.TrimSpace(e.Label), strings.TrimSpace(e.State)
			if label == "" || state == "" {
				continue
			}
			w.agents = append(w.agents, workflowAgent{
				label: label, state: state, phase: e.PhaseIndex, attempt: e.Attempt, tokens: e.Tokens,
				toolCalls: e.ToolCalls, durationMs: e.DurationMs,
				lastTool: strings.TrimSpace(e.LastToolName), lastSummary: strings.TrimSpace(e.LastToolSumry),
			})
		}
	}
	if len(w.phases) == 0 && len(w.agents) == 0 {
		return nil
	}
	return w
}

// text is the snapshot as Markdown: the agents with no known phase, then each
// phase as a heading with its agents below.
func (w *workflowSnapshot) text() string {
	known := map[int]bool{}
	for _, p := range w.phases {
		known[p.index] = true
	}
	var sections []string
	var loose []string
	for _, a := range w.agents {
		if a.phase == nil || !known[*a.phase] {
			loose = append(loose, a.line())
		}
	}
	if len(loose) > 0 {
		sections = append(sections, strings.Join(loose, "\n"))
	}
	for _, p := range w.phases {
		lines := []string{"**" + p.title + "**"}
		n := 0
		for _, a := range w.agents {
			if a.phase != nil && *a.phase == p.index {
				lines = append(lines, a.line())
				n++
			}
		}
		if n == 0 {
			lines = append(lines, "- not started")
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func (a workflowAgent) line() string {
	state := a.state
	if state == "start" {
		state = "running"
	}
	head := a.label + ": " + state
	if a.attempt != nil && *a.attempt > 1 {
		head += fmt.Sprintf(" (attempt %d)", *a.attempt)
	}
	parts := []string{head}
	if a.tokens != nil {
		parts = append(parts, thousands(*a.tokens)+" tokens")
	}
	if a.toolCalls != nil {
		parts = append(parts, thousands(*a.toolCalls)+" tool calls")
	}
	if a.durationMs != nil {
		parts = append(parts, fmt.Sprintf("%ds", *a.durationMs/1000))
	}
	// A finished agent's last tool is history; a running one's is what it does now.
	if a.state == "start" && a.lastTool != "" {
		if a.lastSummary != "" {
			parts = append(parts, a.lastTool+": "+a.lastSummary)
		} else {
			parts = append(parts, a.lastTool)
		}
	}
	return "- " + strings.Join(parts, " · ")
}

// processEnded ends the background work of a process that exited: its tasks
// died with it. Terminals exit without a status, tool calls are cancelled and
// subagents end in error, as claude-agent-acp's queryEnded does. It also
// empties the live set: a process started again by --resume has no tasks.
func (t *translator) processEnded() []backend.Event {
	var out []backend.Event
	for _, ti := range t.tasks {
		if ti.child != nil {
			for _, ev := range ti.child.tr.processEnded() {
				if ev.Session == "" {
					ev.Session = ti.id
				}
				out = append(out, ev)
			}
		}
	}
	for id, ti := range t.tasks {
		if ti.ended || ti.child != nil {
			continue
		}
		out = append(out, t.taskEnded(id, "stopped", "", "")...)
	}
	for id := range t.calls {
		out = append(out, t.callEnded(id, "stopped")...)
	}
	for _, ti := range t.tasks {
		if ti.child != nil && !ti.ended {
			out = append(out, t.taskEnded(ti.id, "failed", "Claude Code stopped", "Claude Code stopped")...)
		}
	}
	t.bg, t.level = nil, map[string]levelTask{}
	return out
}
