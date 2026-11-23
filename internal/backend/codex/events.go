package codex

import (
	"encoding/base64"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type userInput struct {
	Type         string  `json:"type"`
	Text         *string `json:"text,omitempty"`
	TextElements *[]any  `json:"text_elements,omitempty"`
	URL          string  `json:"url,omitempty"`
	Path         string  `json:"path,omitempty"`
}

func promptInput(blocks []acp2.ContentBlock) ([]userInput, error) {
	if len(blocks) == 0 {
		return nil, backend.InvalidParams("prompt must contain at least one block")
	}
	textElements := []any{}
	input := make([]userInput, 0, len(blocks))
	for _, block := range blocks {
		switch v := block.Variant().(type) {
		case schema.ContentBlockText:
			input = append(input, userInput{Type: "text", Text: &v.Text, TextElements: &textElements})
		case schema.ContentBlockImage:
			if !strings.HasPrefix(string(v.MIMEType), "image/") {
				return nil, backend.InvalidParams("invalid image MIME type %q", v.MIMEType)
			}
			input = append(input, userInput{Type: "image", URL: fmt.Sprintf("data:%s;base64,%s", v.MIMEType, v.Data)})
		case schema.ContentBlockResourceLink:
			input = append(input, userInput{Type: "text", Text: new(v.Name + ": " + v.URI), TextElements: &textElements})
		default:
			return nil, backend.InvalidParams("Codex does not support prompt content %q", block.Tag())
		}
	}
	return input, nil
}

type notification struct {
	Turn       turn            `json:"turn"`
	Thread     thread          `json:"thread"`
	TurnID     string          `json:"turnId"`
	ItemID     string          `json:"itemId"`
	Delta      string          `json:"delta"`
	Item       item            `json:"item"`
	RequestID  json.RawMessage `json:"requestId"`
	ThreadName string          `json:"threadName"`
	Plan       []struct {
		Step   string `json:"step"`
		Status string `json:"status"`
	} `json:"plan"`
	TokenUsage struct {
		Last struct {
			InputTokens uint64 `json:"inputTokens"`
		} `json:"last"`
		ModelContextWindow uint64 `json:"modelContextWindow"`
	} `json:"tokenUsage"`
}

type item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	ClientID         string          `json:"clientId"`
	Text             string          `json:"text"`
	Content          json.RawMessage `json:"content"`
	Summary          []string        `json:"summary"`
	Status           string          `json:"status"`
	Command          string          `json:"command"`
	ProcessID        string          `json:"processId"`
	Cwd              string          `json:"cwd"`
	ExitCode         *int64          `json:"exitCode"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	Tool             string          `json:"tool"`
	Query            string          `json:"query"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            json.RawMessage `json:"error"`
	Changes          []fileChange    `json:"changes"`
	SenderThreadID   string          `json:"senderThreadId"`
	Receivers        []string        `json:"receiverThreadIds"`
	AgentThreadID    string          `json:"agentThreadId"`
	AgentPath        string          `json:"agentPath"`
	Kind             string          `json:"kind"`
	AgentsStates     map[string]struct {
		Status string `json:"status"`
	} `json:"agentsStates"`
}

func (s *Session) translate(method string, e notification) {
	switch method {
	case "item/agentMessage/delta":
		if e.ItemID != "" && e.Delta != "" {
			s.deltaItems[e.ItemID] = true
			s.emit(update(schema.SessionUpdateAgentMessageChunk{MessageID: acp2.MessageID(e.ItemID), Content: acp2.TextBlock(e.Delta)}))
		}
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		if e.ItemID != "" && e.Delta != "" {
			s.deltaItems[e.ItemID] = true
			s.emit(update(schema.SessionUpdateAgentThoughtChunk{MessageID: acp2.MessageID(e.ItemID), Content: acp2.TextBlock(e.Delta)}))
		}
	case "item/commandExecution/outputDelta":
		if e.ItemID != "" && e.Delta != "" {
			s.emit(update(schema.SessionUpdateTerminalOutputChunk{TerminalID: schema.TerminalID(e.ItemID), Data: b64(e.Delta)}))
		}
	case "item/plan/delta":
		if e.ItemID != "" && e.Delta != "" {
			s.plans[e.ItemID] += e.Delta
			s.emit(markdownPlan(e.ItemID, s.plans[e.ItemID]))
		}
	case "item/started", "item/completed":
		final := method == "item/completed"
		if e.Item.Type == "userMessage" && s.published[e.Item.ClientID] {
			// The insertion acknowledgement already published this message.
			return
		}
		if e.Item.Cwd == "" {
			e.Item.Cwd = s.cwd
		}
		for _, ev := range e.Item.events(final, s.deltaItems[e.Item.ID]) {
			s.emit(ev)
		}
		s.observeChildren(e.Item)
		if e.Item.Type == "commandExecution" && e.Item.ProcessID != "" {
			s.watchTerminals()
		}
		if final {
			delete(s.deltaItems, e.Item.ID)
			delete(s.plans, e.Item.ID)
		}
	case "turn/plan/updated":
		entries := make([]schema.PlanEntry, 0, len(e.Plan))
		for _, step := range e.Plan {
			status := schema.PlanEntryStatusPending
			switch step.Status {
			case "inProgress":
				status = schema.PlanEntryStatusInProgress
			case "completed":
				status = schema.PlanEntryStatusCompleted
			}
			entries = append(entries, schema.PlanEntry{Content: step.Step, Status: status, Priority: schema.PlanEntryPriorityMedium})
		}
		s.emit(update(schema.SessionUpdatePlanUpdate{Plan: schema.NewPlanUpdateContent(schema.PlanUpdateContentItems{
			PlanID: "codex-checklist", Entries: entries,
		})}))
	case "thread/name/updated":
		if e.ThreadName == "" {
			s.emit(update(schema.SessionUpdateSessionInfoUpdate{Title: optional.Null[string]()}))
		} else {
			s.emit(update(schema.SessionUpdateSessionInfoUpdate{Title: optional.Of(e.ThreadName)}))
		}
	case "thread/tokenUsage/updated":
		if size := e.TokenUsage.ModelContextWindow; size > 0 {
			s.emit(update(schema.SessionUpdateUsageUpdate{Used: e.TokenUsage.Last.InputTokens, Size: size}))
		}
	}
}

func (i item) events(final, streamed bool) []backend.Event {
	if i.ID == "" {
		return nil
	}
	messageID := acp2.MessageID(i.ID)
	switch i.Type {
	case "userMessage":
		if final {
			if i.ClientID != "" {
				messageID = acp2.MessageID(i.ClientID)
			}
			return []backend.Event{update(schema.SessionUpdateUserMessage{MessageID: messageID, Content: optional.Of(historyInput(i.Content))})}
		}
	case "agentMessage":
		if final && (!streamed || i.Text != "") {
			return []backend.Event{update(schema.SessionUpdateAgentMessage{
				MessageID: messageID, Content: optional.Of([]acp2.ContentBlock{acp2.TextBlock(i.Text)}),
			})}
		}
	case "reasoning":
		if final {
			var details []string
			_ = json.Unmarshal(i.Content, &details)
			thought := strings.Join(append(i.Summary, details...), "\n")
			if thought != "" {
				return []backend.Event{update(schema.SessionUpdateAgentThought{
					MessageID: messageID, Content: optional.Of([]acp2.ContentBlock{acp2.TextBlock(thought)}),
				})}
			}
		}
	case "plan":
		if final && i.Text != "" {
			return []backend.Event{markdownPlan(i.ID, i.Text)}
		}
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "collabAgentToolCall", "webSearch":
		return i.toolEvents(final)
	}
	return nil
}

func (i item) toolEvents(final bool) []backend.Event {
	status := schema.ToolCallStatusInProgress
	switch i.Status {
	case "completed":
		status = schema.ToolCallStatusCompleted
	case "failed":
		status = schema.ToolCallStatusFailed
	case "declined", "cancelled", "interrupted":
		status = schema.ToolCallStatusCancelled
	default:
		if final {
			status = schema.ToolCallStatusCompleted
		}
	}
	call := schema.ToolCallUpdate{
		ToolCallID: schema.ToolCallID(i.ID), Name: optional.Of(i.Type), Status: optional.Of(status),
		RawInput: jsontext.Value(i.Arguments), RawOutput: jsontext.Value(i.Result),
	}
	kind, title := schema.ToolKindOther, i.Tool
	var before []backend.Event
	switch i.Type {
	case "commandExecution":
		kind, title = schema.ToolKindExecute, i.Command
		terminalID := schema.TerminalID(i.ID)
		terminal := schema.SessionUpdateTerminalUpdate{TerminalID: terminalID, Command: optional.Of(i.Command)}
		if filepath.IsAbs(i.Cwd) {
			terminal.Cwd = optional.Of(schema.AbsolutePath(i.Cwd))
		}
		if final {
			terminal.ExitStatus = optional.Of(schema.TerminalExitStatus{ExitCode: i.ExitCode})
			if i.AggregatedOutput != nil {
				terminal.Output = optional.Of(schema.TerminalOutput{Data: b64(*i.AggregatedOutput)})
			}
		}
		before = append(before, update(terminal))
		call.Content = optional.Of([]acp2.ToolCallContent{
			schema.NewToolCallContent(schema.ToolCallContentTerminal{TerminalID: terminalID}),
		})
	case "fileChange":
		kind, title = schema.ToolKindEdit, "Apply file changes"
		var locations []schema.ToolCallLocation
		var content []acp2.ToolCallContent
		for _, c := range i.Changes {
			if diff, path, ok := c.content(i.Cwd); ok {
				content = append(content, diff)
				locations = append(locations, schema.ToolCallLocation{Path: path})
			}
		}
		if len(locations) > 0 {
			call.Locations = optional.Of(locations)
		}
		if len(content) > 0 {
			call.Content = optional.Of(content)
		}
	case "webSearch":
		kind, title = schema.ToolKindSearch, i.Query
	}
	if title == "" {
		title = i.Type
	}
	call.Title, call.Kind = optional.Of(title), optional.Of(kind)
	if len(i.Error) > 0 && string(i.Error) != "null" {
		call.RawOutput = jsontext.Value(i.Error)
	}
	return append(before, update(schema.SessionUpdateToolCallUpdate(call)))
}

func markdownPlan(id, text string) backend.Event {
	return update(schema.SessionUpdatePlanUpdate{Plan: schema.NewPlanUpdateContent(schema.PlanUpdateContentMarkdown{
		PlanID: schema.PlanID(id), Content: text,
	})})
}

func historyInput(raw json.RawMessage) []acp2.ContentBlock {
	var input []userInput
	_ = json.Unmarshal(raw, &input)
	out := make([]acp2.ContentBlock, 0, len(input))
	for _, v := range input {
		switch v.Type {
		case "text":
			if v.Text != nil {
				out = append(out, acp2.TextBlock(*v.Text))
			}
		case "image":
			if med, data, ok := strings.Cut(strings.TrimPrefix(v.URL, "data:"), ";base64,"); ok {
				out = append(out, schema.NewContentBlock(schema.ContentBlockImage{Data: data, MIMEType: schema.MediaType(med)}))
			}
		case "localImage", "localAudio", "mention", "skill":
			if v.Path != "" {
				out = append(out, acp2.TextBlock(v.Path))
			}
		}
	}
	return out
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
