package claude

import (
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// The lines below are what Claude Code 2.x printed for an expired login,
// captured from a real run.
const (
	retry401 = `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":536,"error_status":401,"error":"authentication_failed","session_id":"s","uuid":"u"}`
	authMsg  = `{"type":"assistant","message":{"id":"m1","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"Failed to authenticate. API Error: 401 API key expired."}]},"parent_tool_use_id":null,"error":"authentication_failed"}`
	authRes  = `{"type":"result","subtype":"success","is_error":true,"result":"Failed to authenticate. API Error: 401 API key expired.","stop_reason":"stop_sequence","terminal_reason":"api_error","num_turns":1,"api_error_status":401}`
)

func drain(s *Session) []backend.Event {
	var out []backend.Event
	for {
		select {
		case ev := <-s.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func feed(s *Session, p *proc, lines ...string) {
	for _, l := range lines {
		s.handle(p, []byte(l))
	}
}

func TestAPIRetryBecomesNotice(t *testing.T) {
	s := NewSession("id", nil, SpawnOpts{})
	feed(s, &proc{}, retry401)
	evs := drain(s)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 notice", len(evs))
	}
	n, ok := evs[0].Update.Variant().(schema.SessionUpdateNotice)
	if !ok {
		t.Fatalf("event is %T, want a notice", evs[0].Update.Variant())
	}
	if n.Severity != schema.NoticeSeverityWarning || n.Title != "Retrying Claude, attempt 1 of 10." {
		t.Errorf("notice = %+v", n)
	}
	if n.Description == nil || *n.Description != "HTTP 401 (authentication failed). Next attempt in 1s." {
		t.Errorf("description = %v", n.Description)
	}
}

// A failed login ends the turn with authRequired, not an ordinary error, and
// not as text the user sees as Claude's reply.
func TestFailedLoginIsAuthRequired(t *testing.T) {
	s := NewSession("id", nil, SpawnOpts{})
	s.foreground = true // a prompt opened the turn
	feed(s, &proc{}, authMsg, authRes)
	var idle *backend.TurnEvent
	for _, ev := range drain(s) {
		if ev.Kind == backend.EventUpdate {
			if _, ok := ev.Update.Variant().(schema.SessionUpdateAgentMessageChunk); ok {
				t.Error("the synthetic sign-in message was shown as agent text")
			}
		}
		if ev.Kind == backend.EventTurnState && ev.Turn.State == backend.TurnIdle {
			e := ev.Turn
			idle = &e
		}
	}
	if idle == nil {
		t.Fatal("no idle")
	}
	if !acp.IsCode(idle.Err, acp.ErrorCodeAuthRequired) {
		t.Errorf("err = %v, want authRequired", idle.Err)
	}
}

func TestFailureShapes(t *testing.T) {
	// "run /login" advice is dropped from the message.
	err := failure("Not logged in · Please run /login", "", "success")
	if !acp.IsCode(err, acp.ErrorCodeAuthRequired) || contains(err.Error(), "/login") {
		t.Errorf("login text: %v", err)
	}
	// Another failure keeps its kind as data.
	err = failure("API Error: 529 overloaded", "overloaded", "success")
	re, ok := err.(*acp.RequestError)
	if !ok || re.Code != acp.ErrorCodeInternalError {
		t.Fatalf("overloaded: %v", err)
	}
	var data struct {
		ErrorKind string `json:"errorKind"`
	}
	if re.DecodeData(&data) != nil || data.ErrorKind != "overloaded" {
		t.Errorf("data = %+v", data)
	}
	_ = acp2.StopReasonError
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
