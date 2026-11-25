package claude

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"regexp"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// line is the envelope shared by every stdout message. Only the fields the
// translator reads are decoded; everything else stays raw.
type line struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	IsReplay        bool            `json:"isReplay"`
	Event           json.RawMessage `json:"event"`   // stream_event
	Message         json.RawMessage `json:"message"` // assistant / user
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"` // control_request

	// assistant: set on the synthetic message Claude Code writes for a failed
	// API request (authentication_failed, rate_limit, ...).
	Error string `json:"error"`

	UUID        string `json:"uuid"`
	IsSynthetic bool   `json:"isSynthetic"`
	// user: Claude Code's structured form of a tool's result (an Edit's patch,
	// a Bash's stdout, a background task's id, ...).
	ToolUseResult json.RawMessage `json:"tool_use_result"`

	// system/status, system/compact_boundary
	Status        *string `json:"status"`
	CompactResult string  `json:"compact_result"`
	CompactError  string  `json:"compact_error"`
	CompactMeta   struct {
		PostTokens uint64 `json:"post_tokens"`
	} `json:"compact_metadata"`

	// system/task_*, background_tasks_changed
	TaskID         string `json:"task_id"`
	ParentTaskID   string `json:"parent_task_id"`
	ToolUseID      string `json:"tool_use_id"`
	TaskType       string `json:"task_type"`
	SubagentType   string `json:"subagent_type"`
	Description    string `json:"description"`
	Prompt         string `json:"prompt"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	Summary        string `json:"summary"`
	Patch          struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"patch"`
	Tasks []struct {
		TaskID      string `json:"task_id"`
		TaskType    string `json:"task_type"`
		Description string `json:"description"`
		ParentID    string `json:"parent_task_id"`
		Ambient     bool   `json:"ambient"`
	} `json:"tasks"`
	// system/task_progress
	LastToolName     string          `json:"last_tool_name"`
	TaskUsage        *taskUsage      `json:"usage"`
	WorkflowProgress json.RawMessage `json:"workflow_progress"`

	// system/api_retry
	Attempt      int  `json:"attempt"`
	MaxRetries   int  `json:"max_retries"`
	RetryDelayMs int  `json:"retry_delay_ms"`
	ErrorStatus  *int `json:"error_status"`
}

// block is one content block of an assistant or user message.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`   // tool_use
	Name      string          `json:"name"` // tool_use
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result: string or []block
	IsError   bool            `json:"is_error"`
}

type message struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"` // []block, or a string for plain user text
	Usage   *usage          `json:"usage"`
}

type streamEvent struct {
	Type         string  `json:"type"`
	Index        int     `json:"index"`
	Message      message `json:"message"`       // message_start
	ContentBlock block   `json:"content_block"` // content_block_start
	Delta        struct {
		Type     string  `json:"type"`
		Text     string  `json:"text"`
		Thinking string  `json:"thinking"`
		Content  *string `json:"content"` // compaction_delta
	} `json:"delta"`
	Usage *usage `json:"usage"` // message_delta
}

// resultMsg is the message that ends a turn.
type resultMsg struct {
	UUID           string   `json:"uuid"`
	Subtype        string   `json:"subtype"`
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	StopReason     *string  `json:"stop_reason"`
	TerminalReason string   `json:"terminal_reason"`
	Errors         []string `json:"errors"`
	NumTurns       int      `json:"num_turns"`
	TotalCostUSD   *float64 `json:"total_cost_usd"`
	// ModelUsage carries the session's cumulative counters, one entry per
	// model it ran. InputTokens excludes the cache, which is counted on its
	// own; thinking is inside OutputTokens.
	ModelUsage map[string]struct {
		InputTokens              uint64 `json:"inputTokens"`
		OutputTokens             uint64 `json:"outputTokens"`
		CacheReadInputTokens     uint64 `json:"cacheReadInputTokens"`
		CacheCreationInputTokens uint64 `json:"cacheCreationInputTokens"`
		ContextWindow            uint64 `json:"contextWindow"`
	} `json:"modelUsage"`
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
}

// permissionRequest is the can_use_tool control_request body.
type permissionRequest struct {
	Subtype     string         `json:"subtype"`
	ToolName    string         `json:"tool_name"`
	Input       map[string]any `json:"input"`
	ToolUseID   string         `json:"tool_use_id"`
	Title       string         `json:"title"`
	DisplayName string         `json:"display_name"`
	AgentID     string         `json:"agent_id"` // the subagent asking, if one is
	Description string         `json:"description"`
}

// translator turns stdout messages into ACP updates. It keeps only what a
// single assistant message needs: which message streamed its text as deltas
// (so the consolidated copy is not sent twice) and the tools it has announced.
// It is used by one readLoop goroutine and needs no locking.
type translator struct {
	msgID    string
	streamed map[string]bool
	tools    map[string]toolInfo
	// agentText is set once agent text went out in the open turn. A local
	// command (such as /context) answers with its result alone, and the
	// session then reports that text itself.
	agentText bool

	// What the open turn's synthetic messages said about a failed API request:
	// the error kind (authentication_failed, rate_limit, ...) and Claude Code's
	// text for it. Read when the turn's result fails; reset when a turn opens.
	errKind string
	errText string

	// checklist is the session's task list (TaskCreate / TaskUpdate / TaskList).
	checklist taskList
	// usage tracks the context window figures behind usage_update.
	usage usageTracker
	// compaction is the id of the compaction in progress, "" when none.
	compaction string

	sessionID  string // the session this translates; a subagent's has its own
	cwd        string
	terminals  map[string]bool      // Bash tool calls that have a terminal
	tasks      map[string]*taskInfo // background work, by task id
	byUse      map[string]*taskInfo // the same, by the tool call that started it
	launches   map[string]bool      // Agent calls seen, not yet resulted
	orphans    map[string][]*line   // a subagent's lines that came before its task_started
	bg         []string             // task ids of the latest background_tasks_changed
	bgSeen     bool
	advertised map[string]bool         // terminals and tool calls that advertised cancel
	up         *translator             // the translator of the session a subagent's belongs to
	level      map[string]levelTask    // Claude Code's live background tasks (the root's)
	calls      map[string]*taskCall    // tool calls that stand for a task, by task id
	returned   map[string]bool         // tool calls that have their result
	progress   map[string]taskProgress // the latest progress of a task, by task id
	held       map[string]*heldCall    // SendMessage calls not shown to the client
	tails      []tailJob               // output files the session should follow
	stops      []stopJob               // terminals the session should end
	// lastCompaction is the compaction that ended last, for its summary; compacted
	// is set while its compact_boundary is still to come.
	lastCompaction  string
	compacted       bool
	streamedSummary string // summary text already sent as chunks
}

type toolInfo struct {
	name  string
	kind  acp2.ToolKind
	input json.RawMessage // the complete input, once the assistant message has it
}

func newTranslator() *translator {
	return &translator{
		streamed: map[string]bool{}, tools: map[string]toolInfo{},
		terminals: map[string]bool{}, tasks: map[string]*taskInfo{}, byUse: map[string]*taskInfo{},
		launches: map[string]bool{}, orphans: map[string][]*line{}, advertised: map[string]bool{},
		level: map[string]levelTask{}, calls: map[string]*taskCall{}, returned: map[string]bool{},
		progress: map[string]taskProgress{}, held: map[string]*heldCall{},
	}
}

// content translates one stream_event / assistant / user message into content
// updates. It returns nil for messages that carry no user-visible output.
func (t *translator) content(l *line) []backend.Event {
	// Subagent output belongs to the subagent's child session.
	if l.ParentToolUseID != nil {
		return t.childLine(l)
	}
	switch l.Type {
	case "stream_event":
		return t.streamEvent(l.Event)
	case "assistant":
		return t.assistant(l.Message, l.Error)
	case "user":
		if l.IsReplay {
			return nil // an echo of a prompt we already reported ourselves
		}
		if evs := t.compactSummary(l); evs != nil {
			return evs
		}
		return t.toolResults(l)
	}
	return nil
}

func (t *translator) streamEvent(raw json.RawMessage) []backend.Event {
	var e streamEvent
	if json.Unmarshal(raw, &e) != nil {
		return nil
	}
	switch e.Type {
	case "message_start":
		t.msgID = e.Message.ID
		if ev, ok := t.usage.messageStart(e.Message); ok {
			return one(ev)
		}
	case "message_delta":
		if ev, ok := t.usage.messageDelta(e.Usage); ok {
			return one(ev)
		}
	case "content_block_start":
		b := e.ContentBlock
		if b.Type == "compaction" {
			var chunk string
			if json.Unmarshal(b.Content, &chunk) == nil {
				return t.compactionChunk(chunk)
			}
			return nil
		}
		if b.Type == "tool_use" || b.Type == "server_tool_use" {
			t.tools[b.ID] = toolInfo{name: b.Name, kind: toolKind(b.Name)}
			if planTool(b.Name) || isAgentTool(b.Name) || isSendMessage(b.Name) {
				// A plan tool shows as the plan, a subagent as its own session, and a
				// message to a subagent as the message.
				if isSendMessage(b.Name) {
					t.holdSendMessage(b.ID)
				}
				return nil
			}
			// The input is still streaming: announce the call as pending, and
			// complete it from the consolidated assistant message.
			if isBash(b.Name) {
				out := t.bashStart(b.ID)
				return append(out, bashCall(b.ID, b.Name, "", schema.ToolCallStatusPending, nil)...)
			}
			return toolCall(b.ID, b.Name, t.tools[b.ID].kind, schema.ToolCallStatusPending, nil)
		}
	case "content_block_delta":
		switch e.Delta.Type {
		case "text_delta":
			t.streamed[t.msgID] = true
			t.agentText = true
			return one(agentChunk(t.msgID, e.Delta.Text))
		case "thinking_delta":
			t.streamed[t.msgID] = true
			return one(thoughtChunk(t.msgID, e.Delta.Thinking))
		case "compaction_delta":
			if e.Delta.Content != nil {
				return t.compactionChunk(*e.Delta.Content)
			}
		}
	}
	return nil
}

// assistant handles the consolidated message: the text and thinking that did
// not stream (a gateway without streaming), and each tool use with its
// complete input.
func (t *translator) assistant(raw json.RawMessage, errKind string) []backend.Event {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if m.Model == "<synthetic>" {
		// Claude Code's own notices (sign-in, API errors) are not model output.
		// Their text is kept: the turn's failure reports it.
		if errKind != "" {
			t.errKind = errKind
			var blocks []block
			if json.Unmarshal(m.Content, &blocks) == nil {
				var parts []string
				for _, b := range blocks {
					if b.Type == "text" && b.Text != "" {
						parts = append(parts, b.Text)
					}
				}
				if len(parts) > 0 {
					t.errText = strings.Join(parts, "\n")
				}
			}
		}
		return nil
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	t.usage.assistant(m)
	streamed := t.streamed[m.ID]
	delete(t.streamed, m.ID)

	var out []backend.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if !streamed && b.Text != "" {
				t.agentText = true
				out = append(out, agentChunkEv(m.ID, b.Text))
			}
		case "thinking":
			if !streamed && b.Thinking != "" {
				out = append(out, thoughtChunkEv(m.ID, b.Thinking))
			}
		case "tool_use", "server_tool_use":
			info := toolInfo{name: b.Name, kind: toolKind(b.Name), input: b.Input}
			t.tools[b.ID] = info
			if b.Name == "TodoWrite" {
				out = append(out, planUpdate(todoWritePlan(b.Input)))
			}
			if planTool(b.Name) {
				continue
			}
			if isAgentTool(b.Name) {
				t.launches[b.ID] = true
				continue
			}
			if isSendMessage(b.Name) {
				t.holdSendMessage(b.ID)
				continue
			}
			var input map[string]any
			_ = json.Unmarshal(b.Input, &input)
			if isBash(b.Name) {
				out = append(out, t.bashStart(b.ID)...)
				cmd, _ := input["command"].(string)
				if cmd != "" {
					tu := schema.SessionUpdateTerminalUpdate{TerminalID: terminalID(b.ID), Command: optional.Of(cmd)}
					if t.cwd != "" {
						tu.Cwd = optional.Of(schema.AbsolutePath(t.cwd))
					}
					out = append(out, terminalUpdate(tu))
				}
				out = append(out, bashCall(b.ID, toolTitle(b.Name, input), cmd, schema.ToolCallStatusInProgress, b.Input)...)
				continue
			}
			for _, ev := range toolCall(b.ID, toolTitle(b.Name, input), info.kind, schema.ToolCallStatusInProgress, b.Input) {
				if u, ok := ev.Update.Variant().(schema.SessionUpdateToolCallUpdate); ok {
					ev = update(t.withTarget(b.ID, u))
				}
				out = append(out, ev)
			}
		}
	}
	return out
}

// toolResults reports the results of tool uses, which Claude Code adds as a
// user message. A plain-text user message is not output.
func (t *translator) toolResults(l *line) []backend.Event {
	var m message
	if json.Unmarshal(l.Message, &m) != nil {
		return nil
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	var out []backend.Event
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		t.returned[b.ToolUseID] = true
		info := t.tools[b.ToolUseID]
		if isBash(info.name) {
			out = append(out, t.bashResult(b, l, resultText(b.Content))...)
		} else if isAgentTool(info.name) {
			out = append(out, t.agentResult(b, l, info)...)
		} else if isSendMessage(info.name) {
			out = append(out, t.sendMessageResult(b, l)...)
		} else if planTool(info.name) {
			if !b.IsError && applyTaskResult(&t.checklist, info.name, info.input, l.ToolUseResult, resultText(b.Content)) {
				out = append(out, planUpdate(t.checklist.planEntries()))
			}
		} else {
			u := t.withTarget(b.ToolUseID, t.toolResultUpdate(b, l.ToolUseResult))
			if card, held := t.holdCall(b, info, l, len(blocks) == 1); held {
				// The tool call's task goes on: the call stays in progress.
				if st, ok := u.Status.Get(); ok && st == schema.ToolCallStatusCompleted {
					u.Status = optional.Of(schema.ToolCallStatusInProgress)
				}
				out = append(out, update(u))
				out = append(out, card...)
			} else {
				out = append(out, update(u))
			}
		}
		delete(t.tools, b.ToolUseID)
	}
	return out
}

// bashCall names a Bash tool call and shows its terminal. v2 has only updates:
// a field left out keeps the value the client already holds.
func bashCall(id, title, _ string, status acp2.ToolCallStatus, rawInput json.RawMessage) []backend.Event {
	u := schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(id),
		Status:     optional.Of(status),
		Kind:       optional.Of(acp2.ToolKindExecute),
		Content:    optional.Of(terminalContent(id)),
	}
	if title != "" {
		u.Title = optional.Of(title)
	}
	if len(rawInput) > 0 {
		u.RawInput = jsontext.Value(rawInput)
	}
	return one(update(u))
}

// askingSession is the child session a permission request belongs to: the
// announced subagent that asks, or "" for the session itself.
func (t *translator) askingSession(agentID string) string {
	if agentID == "" {
		return ""
	}
	if c := t.childByID(agentID); c != nil {
		return c.id
	}
	return ""
}

// permission translates a can_use_tool control_request.
func (t *translator) permission(l *line) (backend.Event, bool) {
	var r permissionRequest
	if json.Unmarshal(l.Request, &r) != nil || r.Subtype != "can_use_tool" {
		return backend.Event{}, false
	}
	title := r.Title
	if title == "" {
		title = toolTitle(r.ToolName, r.Input)
	}
	return backend.Event{
		Kind: backend.EventPermission,
		Permission: backend.PermissionRequest{
			ReqID:      l.RequestID,
			ToolCallID: r.ToolUseID,
			Title:      title,
			ToolName:   r.ToolName,
			Input:      r.Input,
			// A subagent's request is asked in its own session.
			Session: t.askingSession(r.AgentID),
		},
	}, true
}

// --- result -----------------------------------------------------------------

// isPlaceholder reports a result that ends nothing: Claude Code answers queued
// background-task notifications with one model call and gives each but the last
// a `num_turns: 0` result first.
func (r *resultMsg) isPlaceholder() bool {
	if r.Origin == nil || r.NumTurns != 0 || r.IsError {
		return false
	}
	switch r.Origin.Kind {
	case "task-notification", "peer", "coordinator", "observer", "observer-activity":
		return true
	}
	return false
}

func (r *resultMsg) aborted() bool {
	return r.TerminalReason == "aborted_streaming" || r.TerminalReason == "aborted_tools"
}

// end maps a result to how the turn ended. It follows claude-agent-acp's
// resultEnd: a refusal is a refusal on any subtype, and each subtype decides
// whether is_error fails the work.
func (r *resultMsg) end(cancelled bool) (acp2.StopReason, error) {
	if cancelled || r.aborted() {
		return acp2.StopReasonCancelled, nil
	}
	stop := ""
	if r.StopReason != nil {
		stop = *r.StopReason
	}
	if stop == "refusal" {
		return acp2.StopReasonRefusal, nil
	}
	switch r.Subtype {
	case "success":
		if stop == "max_tokens" {
			return acp2.StopReasonMaxTokens, nil
		}
		if r.IsError {
			return "", fmt.Errorf("%s", firstNonEmpty(r.Result, r.Subtype))
		}
		return acp2.StopReasonEndTurn, nil
	case "error_during_execution":
		if stop == "max_tokens" {
			return acp2.StopReasonMaxTokens, nil
		}
		if r.IsError {
			return "", fmt.Errorf("%s", firstNonEmpty(strings.Join(r.Errors, ", "), r.Subtype))
		}
		return acp2.StopReasonEndTurn, nil
	default: // error_max_turns, error_max_budget_usd, error_max_structured_output_retries
		if r.IsError {
			return "", fmt.Errorf("%s", firstNonEmpty(strings.Join(r.Errors, ", "), r.Subtype))
		}
		return acp2.StopReasonMaxTurnRequests, nil
	}
}

// loginHint matches Claude Code's "run /login" instruction, which only means
// something in its own terminal.
var loginHint = regexp.MustCompile(`(?i)\brun /login\b`)

// failure turns a failed turn's error text into the error idle carries,
// following claude-agent-acp's outcome.ts. A sign-out is authRequired (-32000),
// so the client can start its own login; anything else is an internal error
// whose data names the error kind. Claude Code's "/login" advice is left out.
func failure(text, kind, subtype string) error {
	// Only these subtypes read the assistant's error kind (as outcome.ts does).
	kindApplies := subtype == "success" || subtype == "error_during_execution"
	loginText := loginHint.MatchString(text)
	if loginText || (kindApplies && (kind == "authentication_failed" || kind == "oauth_org_not_allowed")) {
		detail := text
		if loginText {
			detail = ""
		}
		return acp.AuthRequired(detail)
	}
	err := acp.InternalError(text)
	if kindApplies && kind != "" {
		err = err.WithData(map[string]any{"errorKind": kind})
	}
	return err
}

// retryNotice reports Claude Code's api_retry as a notice: the attempt, the
// cause and the wait. It follows claude-agent-acp's apiRetryWarning.
func retryNotice(l *line) backend.Event {
	title := fmt.Sprintf("Retrying Claude, attempt %d of %d.", l.Attempt, l.MaxRetries)
	cause := "Connection error: no HTTP response."
	label := retryKindLabels[l.Error]
	if l.ErrorStatus != nil {
		title = fmt.Sprintf("Retrying Claude, attempt %d of %d.", l.Attempt, l.MaxRetries)
		cause = fmt.Sprintf("HTTP %d", *l.ErrorStatus)
		if label != "" {
			cause += " (" + label + ")"
		}
		cause += "."
	} else {
		title = fmt.Sprintf("Reconnecting to Claude, attempt %d of %d.", l.Attempt, l.MaxRetries)
		if label != "" {
			cause = "No HTTP status (" + label + ")."
		}
	}
	desc := cause + " Next attempt " + nextAttempt(l.RetryDelayMs) + "."
	return update(schema.SessionUpdateNotice{
		Severity:    schema.NoticeSeverityWarning,
		Title:       title,
		Description: &desc,
	})
}

var retryKindLabels = map[string]string{
	"authentication_failed":  "authentication failed",
	"oauth_org_not_allowed":  "organization not allowed",
	"account_on_hold":        "account on hold",
	"verification_required":  "verification required",
	"billing_error":          "billing error",
	"rate_limit":             "rate limited",
	"overloaded":             "overloaded",
	"invalid_request":        "invalid request",
	"model_not_found":        "model not found",
	"server_error":           "server error",
	"max_output_tokens":      "output token limit",
	"cloud_credential_error": "cloud credential error",
}

// nextAttempt words the wait before the next try: "now", "in 8s", "in 3m".
func nextAttempt(ms int) string {
	sec := (ms + 500) / 1000
	switch {
	case sec <= 0:
		return "now"
	case sec < 120:
		return fmt.Sprintf("in %ds", sec)
	}
	return fmt.Sprintf("in %dm", (ms+59999)/60000)
}

// --- update builders --------------------------------------------------------

func update[T schema.SessionUpdateVariants](v T) backend.Event {
	return backend.Event{Kind: backend.EventUpdate, Update: schema.NewSessionUpdate(v)}
}

func one(e backend.Event) []backend.Event { return []backend.Event{e} }

func agentChunkEv(id, text string) backend.Event {
	return update(schema.SessionUpdateAgentMessageChunk{MessageID: acp2.MessageID(id), Content: acp2.TextBlock(text)})
}

func agentChunk(id, text string) backend.Event { return agentChunkEv(id, text) }

func thoughtChunkEv(id, text string) backend.Event {
	return update(schema.SessionUpdateAgentThoughtChunk{MessageID: acp2.MessageID(id), Content: acp2.TextBlock(text)})
}

func thoughtChunk(id, text string) backend.Event { return thoughtChunkEv(id, text) }

// toolCall builds a tool_call_update that names the call. v2 has only updates:
// a field left out keeps the value the client already holds.
func toolCall(id, title string, kind acp2.ToolKind, status acp2.ToolCallStatus, rawInput json.RawMessage) []backend.Event {
	u := schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(id),
		Status:     optional.Of(status),
		Title:      optional.Of(title),
		Kind:       optional.Of(kind),
	}
	if len(rawInput) > 0 {
		u.RawInput = jsontext.Value(rawInput)
	}
	return one(update(u))
}

// UserMessage builds the user_message update that reports an accepted prompt.
func userMessage(id string, content []acp2.ContentBlock) backend.Event {
	return update(schema.SessionUpdateUserMessage{MessageID: acp2.MessageID(id), Content: optional.Of(content)})
}

// --- helpers ----------------------------------------------------------------

func toolKind(name string) acp2.ToolKind {
	switch name {
	case "Read", "NotebookRead":
		return acp2.ToolKindRead
	case "Edit", "Write", "NotebookEdit", "MultiEdit":
		return acp2.ToolKindEdit
	case "Glob", "Grep", "ToolSearch":
		return acp2.ToolKindSearch
	case "Bash", "BashOutput", "KillShell":
		return acp2.ToolKindExecute
	case "WebFetch", "WebSearch":
		return acp2.ToolKindFetch
	case "ExitPlanMode":
		return acp2.ToolKindSwitchMode
	case "Task", "Agent", "TodoWrite":
		return acp2.ToolKindThink
	}
	return acp2.ToolKindOther
}

func toolTitle(name string, in map[string]any) string {
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Bash":
		return firstNonEmpty(str("description"), str("command"), name)
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		if p := str("file_path"); p != "" {
			return name + " " + p
		}
	case "Glob", "Grep":
		if p := str("pattern"); p != "" {
			return name + " " + p
		}
	case "WebFetch":
		if u := str("url"); u != "" {
			return "Fetch " + u
		}
	case "WebSearch":
		if q := str("query"); q != "" {
			return "Search " + q
		}
	}
	return name
}

// resultText flattens a tool_result's content: a string, or text blocks.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
