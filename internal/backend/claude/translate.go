package claude

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
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
}

type streamEvent struct {
	Type         string  `json:"type"`
	Index        int     `json:"index"`
	Message      message `json:"message"`       // message_start
	ContentBlock block   `json:"content_block"` // content_block_start
	Delta        struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
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
	Origin         *struct {
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
}

type toolInfo struct {
	name string
	kind acp2.ToolKind
}

func newTranslator() *translator {
	return &translator{streamed: map[string]bool{}, tools: map[string]toolInfo{}}
}

// content translates one stream_event / assistant / user message into content
// updates. It returns nil for messages that carry no user-visible output.
func (t *translator) content(l *line) []backend.Event {
	// Subagent output belongs to a child session, which is not modelled yet.
	if l.ParentToolUseID != nil {
		return nil
	}
	switch l.Type {
	case "stream_event":
		return t.streamEvent(l.Event)
	case "assistant":
		return t.assistant(l.Message)
	case "user":
		if l.IsReplay {
			return nil // an echo of a prompt we already reported ourselves
		}
		return t.toolResults(l.Message)
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
	case "content_block_start":
		b := e.ContentBlock
		if b.Type == "tool_use" || b.Type == "server_tool_use" {
			// The input is still streaming: announce the call as pending, and
			// complete it from the consolidated assistant message.
			info := toolInfo{name: b.Name, kind: toolKind(b.Name)}
			t.tools[b.ID] = info
			return toolCall(b.ID, info.name, info.kind, schema.ToolCallStatusPending, nil)
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
		}
	}
	return nil
}

// assistant handles the consolidated message: the text and thinking that did
// not stream (a gateway without streaming), and each tool use with its
// complete input.
func (t *translator) assistant(raw json.RawMessage) []backend.Event {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if m.Model == "<synthetic>" {
		return nil // Claude Code's own notices (sign-in, API errors) are not model output
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
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
			var input map[string]any
			_ = json.Unmarshal(b.Input, &input)
			info := toolInfo{name: b.Name, kind: toolKind(b.Name)}
			t.tools[b.ID] = info
			upd := toolCall(b.ID, toolTitle(b.Name, input), info.kind, schema.ToolCallStatusInProgress, b.Input)
			out = append(out, upd...)
		}
	}
	return out
}

// toolResults reports the results of tool uses, which Claude Code adds as a
// user message. A plain-text user message is not output.
func (t *translator) toolResults(raw json.RawMessage) []backend.Event {
	var m message
	if json.Unmarshal(raw, &m) != nil {
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
		status := schema.ToolCallStatusCompleted
		if b.IsError {
			status = schema.ToolCallStatusFailed
		}
		upd := schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(b.ToolUseID),
			Status:     optional.Of(status),
		}
		if text := resultText(b.Content); text != "" {
			upd.Content = optional.Of([]acp2.ToolCallContent{acp2.ToolText(text)})
		}
		out = append(out, update(upd))
		delete(t.tools, b.ToolUseID)
	}
	return out
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
