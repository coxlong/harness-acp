package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// These tests drive the real claude CLI and cost a little each run:
//
//	CLAUDE_E2E=1 go test ./internal/backend/claude -run E2E -v
func e2e(t *testing.T) (*Backend, string) {
	t.Helper()
	if os.Getenv("CLAUDE_E2E") == "" {
		t.Skip("set CLAUDE_E2E=1 to run against the real claude CLI")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := NewBackend(NewRuntime(ctx, RuntimeConfig{}))
	t.Cleanup(func() { _ = b.Close() })
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b, dir
}

// until reads events until stop returns true, and returns everything read.
func until(t *testing.T, s backend.SessionHandle, stop func(backend.Event) bool) []backend.Event {
	t.Helper()
	var got []backend.Event
	timeout := time.After(120 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed early; got %s", describe(got))
			}
			got = append(got, ev)
			if stop(ev) {
				return got
			}
		case <-timeout:
			t.Fatalf("timed out; got %s", describe(got))
		}
	}
}

func isIdle(ev backend.Event) bool {
	return ev.Kind == backend.EventTurnState && ev.Turn.State == backend.TurnIdle
}

func describe(evs []backend.Event) string {
	var parts []string
	for _, ev := range evs {
		switch ev.Kind {
		case backend.EventUpdate:
			parts = append(parts, "update:"+kindOf(ev.Update))
		case backend.EventTurnState:
			parts = append(parts, map[backend.TurnState]string{
				backend.TurnRunning: "running", backend.TurnRequiresAction: "requires_action", backend.TurnIdle: "idle:" + string(ev.Turn.StopReason),
			}[ev.Turn.State])
		case backend.EventPermission:
			parts = append(parts, "permission:"+ev.Permission.ToolName)
		case backend.EventFatal:
			parts = append(parts, "fatal")
		}
	}
	return strings.Join(parts, " ")
}

func kindOf(u acp2.SessionUpdate) string {
	switch u.Variant().(type) {
	case schema.SessionUpdateUserMessage:
		return "user_message"
	case schema.SessionUpdateAgentMessageChunk:
		return "agent_chunk"
	case schema.SessionUpdateAgentThoughtChunk:
		return "thought_chunk"
	case schema.SessionUpdateToolCallUpdate:
		return "tool_call"
	}
	return "other"
}

func agentText(evs []backend.Event) string {
	var sb strings.Builder
	for _, ev := range evs {
		if ev.Kind != backend.EventUpdate {
			continue
		}
		if c, ok := ev.Update.Variant().(schema.SessionUpdateAgentMessageChunk); ok {
			if txt, ok := acp2.TextOf(c.Content); ok {
				sb.WriteString(txt)
			}
		}
	}
	return sb.String()
}

func text(s string) []acp2.ContentBlock { return []acp2.ContentBlock{acp2.TextBlock(s)} }

// A turn reports user_message, then running, then output, then idle(end_turn),
// in that order. This is the contract the v2 client relies on to know a turn ended.
func TestE2E_TurnLifecycle(t *testing.T) {
	b, dir := e2e(t)
	s, err := b.NewSession(context.Background(), backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	if _, err := s.Send(context.Background(), text("reply with exactly: pong")); err != nil {
		t.Fatal(err)
	}
	got := until(t, s, isIdle)
	t.Log(describe(got))

	if kindOfEv(got[0]) != "user_message" {
		t.Errorf("first event = %s, want user_message", describe(got[:1]))
	}
	if got[1].Kind != backend.EventTurnState || got[1].Turn.State != backend.TurnRunning {
		t.Errorf("second event = %s, want running", describe(got[1:2]))
	}
	last := got[len(got)-1]
	if last.Turn.StopReason != acp2.StopReasonEndTurn || last.Turn.Err != nil {
		t.Errorf("idle = %q err=%v, want end_turn", last.Turn.StopReason, last.Turn.Err)
	}
	if !strings.Contains(strings.ToLower(agentText(got)), "pong") {
		t.Errorf("agent text = %q, want it to contain pong", agentText(got))
	}
	// Streamed deltas and the consolidated message must not both be sent.
	if n := strings.Count(strings.ToLower(agentText(got)), "pong"); n != 1 {
		t.Errorf("pong appears %d times, want 1 (text sent twice?)", n)
	}
}

func kindOfEv(ev backend.Event) string {
	if ev.Kind != backend.EventUpdate {
		return "-"
	}
	return kindOf(ev.Update)
}

// A tool that needs permission blocks the turn on the user: requires_action,
// then the request, then running once answered.
func TestE2E_Permission(t *testing.T) {
	b, dir := e2e(t)
	s, err := b.NewSession(context.Background(), backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	target := filepath.Join(dir, "x.txt")
	if _, err := s.Send(context.Background(), text("Create the file "+target+" containing the single word hi, using the Write tool")); err != nil {
		t.Fatal(err)
	}
	pre := until(t, s, func(ev backend.Event) bool { return ev.Kind == backend.EventPermission })
	t.Log(describe(pre))
	req := pre[len(pre)-1].Permission
	if req.ToolName != "Write" || req.ToolCallID == "" || req.ReqID == "" {
		t.Fatalf("bad permission request: %+v", req)
	}
	sawRequires := false
	for _, ev := range pre {
		if ev.Kind == backend.EventTurnState && ev.Turn.State == backend.TurnRequiresAction {
			sawRequires = true
		}
	}
	if !sawRequires {
		t.Error("no requires_action before the permission request")
	}

	if err := s.Respond(req.ReqID, backend.Decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	post := until(t, s, isIdle)
	t.Log(describe(post))
	if post[0].Kind != backend.EventTurnState || post[0].Turn.State != backend.TurnRunning {
		t.Errorf("after answering, first event = %s, want running", describe(post[:1]))
	}
	if idle := post[len(post)-1]; idle.Turn.StopReason != acp2.StopReasonEndTurn {
		t.Errorf("idle = %q err=%v", idle.Turn.StopReason, idle.Turn.Err)
	}
	if b, err := os.ReadFile(target); err != nil || !strings.Contains(string(b), "hi") {
		t.Errorf("file not written: %v %q", err, b)
	}
}

// Cancelling ends the turn with idle(cancelled), not an error.
func TestE2E_Cancel(t *testing.T) {
	b, dir := e2e(t)
	s, err := b.NewSession(context.Background(), backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	if _, err := s.Send(context.Background(), text("Count slowly from 1 to 300, one number per line.")); err != nil {
		t.Fatal(err)
	}
	until(t, s, func(ev backend.Event) bool { return kindOfEv(ev) == "agent_chunk" })
	if err := s.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := until(t, s, isIdle)
	last := got[len(got)-1]
	t.Log(describe(got))
	if last.Turn.StopReason != acp2.StopReasonCancelled || last.Turn.Err != nil {
		t.Errorf("idle = %q err=%v, want cancelled", last.Turn.StopReason, last.Turn.Err)
	}
}

// A session can be reopened from its transcript: History replays what was said,
// and a prompt after the resume (a fresh process, --resume) works.
func TestE2E_ResumeAndHistory(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	id := s.NativeID()
	if _, err := s.Send(ctx, text("reply with exactly: pong")); err != nil {
		t.Fatal(err)
	}
	until(t, s, isIdle)
	_ = s.Close(ctx)

	hist, err := b.History(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var sawUser, sawAgent bool
	for _, u := range hist {
		switch v := u.Variant().(type) {
		case schema.SessionUpdateUserMessage:
			sawUser = true
		case schema.SessionUpdateAgentMessage:
			if c, ok := v.Content.Get(); ok && len(c) > 0 {
				sawAgent = true
			}
		}
	}
	if !sawUser || !sawAgent {
		t.Errorf("history user=%v agent=%v (%d updates)", sawUser, sawAgent, len(hist))
	}

	list, _, err := b.ListSessions(ctx, backend.ListFilter{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, si := range list {
		if si.ID == id {
			found = true
			if !strings.Contains(si.Title, "pong") {
				t.Errorf("title = %q, want the first prompt", si.Title)
			}
		}
	}
	if !found {
		t.Errorf("session %s not listed (%d listed)", id, len(list))
	}

	r, err := b.ResumeSession(ctx, id, backend.ResumeParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if _, err := r.Send(ctx, text("reply with exactly: pong2")); err != nil {
		t.Fatal(err)
	}
	got := until(t, r, isIdle)
	t.Log(describe(got))
	if last := got[len(got)-1]; last.Turn.StopReason != acp2.StopReasonEndTurn {
		t.Errorf("resumed turn idle = %q err=%v", last.Turn.StopReason, last.Turn.Err)
	}
}
