package claude

import (
	"encoding/json"
	"encoding/json/jsontext"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// A message to a subagent. Claude Code runs a finished subagent again when a
// SendMessage call names it: the same task id starts once more, under the
// SendMessage call's id, while the subagent's messages still carry the call
// that launched it. As claude-agent-acp does, that is the same child session
// running again, and the SendMessage call is the message it was sent (the
// sender's outgoing view and the child's incoming one) rather than a tool
// call. A call whose message reaches no announced child is shown after all,
// with its final status. Follows v2/tasks.ts (sendMessageUpdate, startRun,
// sendMessageResumed, withTarget).

// heldCall is a SendMessage call not shown to the client yet.
type heldCall struct {
	shown     bool // a permission request showed it
	delivered bool // its message reached a child
}

func isSendMessage(name string) bool { return name == "SendMessage" }

// holdSendMessage keeps a SendMessage call back until its fate is known.
func (t *translator) holdSendMessage(id string) {
	if t.held[id] == nil {
		t.held[id] = &heldCall{}
	}
}

// reveal shows a held call that a permission request is about: the client
// cannot be asked about a tool call it does not know.
func (t *translator) reveal(id string) []backend.Event {
	h := t.held[id]
	if h != nil && !h.shown {
		h.shown = true
		return t.initialCall(id)
	}
	for _, ti := range t.tasks {
		if ti.child != nil {
			var out []backend.Event
			for _, ev := range ti.child.tr.reveal(id) {
				ev.Session = ti.id
				out = append(out, ev)
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

// initialCall is the tool call of a tool use that was held back.
func (t *translator) initialCall(id string) []backend.Event {
	info := t.tools[id]
	var input map[string]any
	_ = json.Unmarshal(info.input, &input)
	u := t.withTarget(id, schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(id),
		Status:     optional.Of(schema.ToolCallStatusInProgress),
		Title:      optional.Of(toolTitle(info.name, input)),
		Kind:       optional.Of(info.kind),
	})
	if len(info.input) > 0 {
		u.RawInput = jsontext.Value(info.input)
	}
	return one(update(u))
}

// withTarget adds the reference a TaskStop or SendMessage call carries to the
// subagent session it names, when that is a session other than the one the
// call is shown in. Only an update that carries content, or opens the call,
// gets it: an update without content keeps what the client holds.
func (t *translator) withTarget(id string, u schema.SessionUpdateToolCallUpdate) schema.SessionUpdateToolCallUpdate {
	info := t.tools[id]
	agent := targetAgent(info.name, info.input)
	if agent == "" || agent == t.sessionID {
		return u
	}
	c := t.childByID(agent)
	if c == nil {
		return u
	}
	content, has := u.Content.Get()
	if !has && !hasTitle(u) {
		return u
	}
	for _, x := range content {
		if s, ok := x.As[schema.ToolCallContentSession](); ok && string(s.SessionID) == c.id {
			return u
		}
	}
	u.Content = optional.Of(append(content, schema.NewToolCallContent(schema.ToolCallContentSession{SessionID: schema.SessionID(c.id)})))
	return u
}

// sendMessageResult reports the result of a SendMessage call: nothing when its
// message reached a child (it is that message), else the call with its final
// status.
func (t *translator) sendMessageResult(b block, l *line) []backend.Event {
	h := t.held[b.ToolUseID]
	defer delete(t.held, b.ToolUseID)
	if h == nil {
		h = &heldCall{shown: true}
	}
	// Claude Code can resume an agent without a new task_started.
	var res struct {
		Success bool   `json:"success"`
		Resumed string `json:"resumedAgentId"`
	}
	var out []backend.Event
	if len(l.ToolUseResult) > 0 && l.ToolUseResult[0] == '{' && json.Unmarshal(l.ToolUseResult, &res) == nil && res.Success && res.Resumed != "" && !h.delivered {
		if c := t.childByID(res.Resumed); c != nil {
			if ti := t.tasks[c.id]; ti != nil {
				out = append(out, t.resume(ti, b.ToolUseID, "")...)
				h.delivered = true
			}
		}
	}
	if h.delivered && !h.shown {
		return out
	}
	if !h.shown {
		out = append(out, t.initialCall(b.ToolUseID)...)
	}
	return append(out, update(t.withTarget(b.ToolUseID, t.toolResultUpdate(b, l.ToolUseResult))))
}

// resume starts a run of an announced subagent again, under the call `use`
// that sent it `prompt` (or, for a SendMessage call, its message).
func (t *translator) resume(ti *taskInfo, use, prompt string) []backend.Event {
	c := ti.child
	ti.ended, c.ended = false, false
	ti.runUse = use
	if use != "" {
		t.byUse[use] = ti
	}
	// The child's state differs from what was last reported: it runs again.
	c.stateKey = "resumed"
	out := t.childState(c)

	var sender string
	h := t.held[use]
	if h != nil {
		var in struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(t.tools[use].input, &in)
		prompt = firstNonEmpty(in.Message, prompt)
		h.delivered = true
		sender = t.sessionID
	}
	if prompt == "" {
		return out
	}
	if h != nil && !h.shown {
		o, in := linked(use, sender, c.id, prompt)
		return append(append(out, update(o)), childEvent(c.id, update(in)))
	}
	msg := schema.SessionUpdateSessionMessage{
		MessageID:          schema.MessageID(use),
		RecipientSessionID: optional.Of(schema.SessionID(c.id)),
		Content:            optional.Of([]acp2.ContentBlock{acp2.TextBlock(prompt)}),
	}
	if sender != "" {
		msg.SenderSessionID = optional.Of(schema.SessionID(sender))
	}
	return append(out, childEvent(c.id, update(msg)))
}
