package codex

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func TestCodexCommandsIncludesBuiltinsAndSkills(t *testing.T) {
	desc := "Full description"
	short := "Short skill summary"
	cmds := codexCommands([]skillInfo{
		{Name: "deploy", Description: &desc, ShortDescription: &short},
		{Name: "bad skill", Description: &desc},
		{Name: "deploy", Description: &desc},
	})
	var names []string
	for _, c := range cmds {
		cmd, ok := c.As[schema.AvailableCommandCommand]()
		if !ok {
			t.Fatalf("unexpected variant %T", c.Variant())
		}
		names = append(names, cmd.Name)
		if cmd.Name == "$deploy" && cmd.Description != short {
			t.Fatalf("skill desc = %q, want %q", cmd.Description, short)
		}
	}
	for _, want := range []string{"plan", "mcp", "skills", "status", "review", "review-branch", "review-commit", "compact", "rename", "$deploy"} {
		if !slices.Contains(names, want) {
			t.Fatalf("missing command %q in %v", want, names)
		}
	}
	if slices.Contains(names, "$bad skill") {
		t.Fatalf("whitespace command should be excluded: %v", names)
	}
}

func TestCompactSlashCommandCallsThreadCompactStartAndMapsLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), rpcTimeout)
	defer cancel()
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 8),
		pending: make(map[uint64]chan message), sessions: make(map[string]*Session),
	}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1", Cwd: "/work"}})
	if _, err := p.put(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.end(nil) })

	type sent struct {
		id  acp2.MessageID
		err error
	}
	resCh := make(chan sent, 1)
	go func() {
		id, err := s.Send(ctx, []acp2.ContentBlock{acp2.TextBlock("/compact")})
		resCh <- sent{id, err}
	}()

	var req message
	select {
	case raw := <-p.writes:
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for thread/compact/start")
	}
	if req.Method != "thread/compact/start" {
		t.Fatalf("method = %q, want thread/compact/start", req.Method)
	}

	// Simulate early notifications arriving before the RPC reply (staged).
	p.dispatch(message{
		Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-c1","status":"inProgress"}}`),
	})
	p.dispatch(message{
		Method: "item/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-c1","item":{"id":"cmp-1","type":"contextCompaction"}}`),
	})
	p.deliverReply(message{ID: req.ID, Result: json.RawMessage(`{}`)})

	res := <-resCh
	if res.err != nil {
		t.Fatal(res.err)
	}

	readEvent := func() backend.Event {
		t.Helper()
		select {
		case ev := <-s.Events():
			return ev
		case <-ctx.Done():
			t.Fatal("timed out waiting for event")
			return backend.Event{}
		}
	}

	if u, ok := readEvent().Update.Variant().(schema.SessionUpdateUserMessage); !ok || u.MessageID != res.id {
		t.Fatalf("want user_message %q, got %#v", res.id, u)
	}
	if ev := readEvent(); ev.Kind != backend.EventTurnState || ev.Turn.State != backend.TurnRunning {
		t.Fatalf("want running, got %#v", ev)
	}
	if cmp, ok := readEvent().Update.Variant().(schema.SessionUpdateCompactionUpdate); !ok || cmp.CompactionID != "cmp-1" || cmp.Status != schema.CompactionStatusInProgress {
		t.Fatalf("want compaction in_progress, got %#v", cmp)
	}

	// Complete compaction, send duplicate legacy signal, then end turn.
	p.dispatch(message{
		Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-c1","item":{"id":"cmp-1","type":"contextCompaction"}}`),
	})
	p.dispatch(message{
		Method: "thread/compacted",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-c1"}`),
	})
	p.dispatch(message{
		Method: "turn/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-c1","status":"completed"}}`),
	})

	if cmp, ok := readEvent().Update.Variant().(schema.SessionUpdateCompactionUpdate); !ok || cmp.CompactionID != "cmp-1" || cmp.Status != schema.CompactionStatusCompleted {
		t.Fatalf("want compaction completed, got %#v", cmp)
	}
	// Duplicate legacy signal must not emit a second compaction_update; next event is idle.
	if ev := readEvent(); ev.Kind != backend.EventTurnState || ev.Turn.State != backend.TurnIdle || ev.Turn.StopReason != acp2.StopReasonEndTurn {
		t.Fatalf("want idle(end_turn), got %#v", ev)
	}
}

func TestLocalSlashCommandsStatusAndPlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), rpcTimeout)
	defer cancel()
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 4),
		pending: make(map[uint64]chan message), sessions: make(map[string]*Session),
	}
	s := newSession(p, threadResponse{
		Thread: thread{ID: "thread-1", Cwd: "/work"},
		Model:  "gpt-5",
	})
	s.models = []model{{ID: "gpt-5", DisplayName: "GPT-5"}}
	s.lastUsage = &tokenUsage{
		ModelContextWindow: 100000,
		Last:               tokenCounters{TotalTokens: 25000},
		Total:              tokenCounters{TotalTokens: 40000, InputTokens: 30000, CachedInputTokens: 5000, OutputTokens: 5000},
	}
	t.Cleanup(func() { s.end(nil) })

	// /status runs locally: user_message -> running -> agent_message -> idle(end_turn).
	id, err := s.Send(ctx, []acp2.ContentBlock{acp2.TextBlock("/status")})
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := (<-s.Events()).Update.Variant().(schema.SessionUpdateUserMessage); !ok || u.MessageID != id {
		t.Fatalf("want user_message, got %#v", u)
	}
	if ev := <-s.Events(); ev.Kind != backend.EventTurnState || ev.Turn.State != backend.TurnRunning {
		t.Fatalf("want running, got %#v", ev)
	}
	msg, ok := (<-s.Events()).Update.Variant().(schema.SessionUpdateAgentMessage)
	if !ok {
		t.Fatalf("want agent_message, got %#v", msg)
	}
	blocks, _ := msg.Content.Get()
	text := acp2.JoinTexts(blocks)
	if !strings.Contains(text, "gpt-5") || !strings.Contains(text, "25% used") {
		t.Fatalf("unexpected /status reply: %q", text)
	}
	if ev := <-s.Events(); ev.Kind != backend.EventTurnState || ev.Turn.State != backend.TurnIdle || ev.Turn.StopReason != acp2.StopReasonEndTurn {
		t.Fatalf("want idle(end_turn), got %#v", ev)
	}

	// /plan toggles collaboration_mode to plan locally without deadlock.
	if _, err := s.Send(ctx, []acp2.ContentBlock{acp2.TextBlock("/plan")}); err != nil {
		t.Fatal(err)
	}
	var sawConfig bool
	for range 4 {
		ev := <-s.Events()
		if cfg, ok := ev.Update.Variant().(schema.SessionUpdateConfigOptionUpdate); ok {
			sawConfig = true
			for _, o := range cfg.ConfigOptions {
				if sel, ok := o.As[schema.SessionConfigOptionSelect](); ok && sel.ConfigID == "collaboration_mode" {
					if sel.CurrentValue != "plan" {
						t.Fatalf("collaboration_mode = %q, want plan", sel.CurrentValue)
					}
				}
			}
		}
	}
	if !sawConfig {
		t.Fatal("expected config_option_update from /plan")
	}
}

func TestContextCompactionHistoryReplay(t *testing.T) {
	evs := (item{ID: "cmp-hist", Type: "contextCompaction"}).events(true, false, optional.Value[schema.Meta]{})
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	cmp, ok := evs[0].Update.Variant().(schema.SessionUpdateCompactionUpdate)
	if !ok || cmp.CompactionID != "cmp-hist" || cmp.Status != schema.CompactionStatusCompleted {
		t.Fatalf("unexpected compaction replay: %#v", cmp)
	}
}

func TestReviewParentAndChildTurnIDsCompleteAndInterrupt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), rpcTimeout)
	defer cancel()
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 8),
		pending: make(map[uint64]chan message), sessions: make(map[string]*Session),
	}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1", Cwd: "/work"}})
	if _, err := p.put(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.end(nil) })

	doneSend := make(chan error, 1)
	go func() {
		_, err := s.Send(ctx, []acp2.ContentBlock{acp2.TextBlock("/review")})
		doneSend <- err
	}()

	var req message
	if err := json.Unmarshal(<-p.writes, &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != "review/start" {
		t.Fatalf("method = %q, want review/start", req.Method)
	}
	// Codex responds with parentTurnId, then emits turn/started with childTurnId.
	p.deliverReply(message{ID: req.ID, Result: json.RawMessage(`{"reviewThreadId":"thread-1","turn":{"id":"review-parent","status":"inProgress"}}`)})
	if err := <-doneSend; err != nil {
		t.Fatal(err)
	}
	<-s.Events() // user_message
	<-s.Events() // running

	p.dispatch(message{
		Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"review-child","status":"inProgress"}}`),
	})

	// Cancel must interrupt the child turn id, not the parent id.
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- s.Cancel(ctx) }()
	var intReq message
	if err := json.Unmarshal(<-p.writes, &intReq); err != nil {
		t.Fatal(err)
	}
	if intReq.Method != "turn/interrupt" || !strings.Contains(string(intReq.Params), `"turnId":"review-child"`) {
		t.Fatalf("unexpected interrupt request: method=%q params=%s", intReq.Method, intReq.Params)
	}
	p.deliverReply(message{ID: intReq.ID, Result: json.RawMessage(`{}`)})
	if err := <-cancelDone; err != nil {
		t.Fatal(err)
	}

	// Codex reports completion under the parent turn id; session must return to idle.
	p.dispatch(message{
		Method: "turn/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"review-parent","status":"interrupted"}}`),
	})
	ev := <-s.Events()
	if ev.Kind != backend.EventTurnState || ev.Turn.State != backend.TurnIdle || ev.Turn.StopReason != acp2.StopReasonCancelled {
		t.Fatalf("want idle(cancelled), got %#v", ev)
	}
}

func TestLocalCommandErrorSurfacedDuringActiveTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), rpcTimeout)
	defer cancel()
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 4),
		pending: make(map[uint64]chan message), sessions: make(map[string]*Session),
	}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1", Cwd: "/work"}})
	s.mu.Lock()
	s.startTurn("turn-active")
	s.mu.Unlock()
	<-s.Events() // drain running
	t.Cleanup(func() { s.end(nil) })

	doneSend := make(chan error, 1)
	go func() {
		_, err := s.Send(ctx, []acp2.ContentBlock{acp2.TextBlock("/rename new-title")})
		doneSend <- err
	}()
	var req message
	if err := json.Unmarshal(<-p.writes, &req); err != nil {
		t.Fatal(err)
	}
	p.deliverReply(message{ID: req.ID, Error: &rpcError{Code: -32000, Message: "rename failed"}})
	if err := <-doneSend; err != nil {
		t.Fatal(err)
	}
	<-s.Events() // user_message
	ev := <-s.Events()
	msg, ok := ev.Update.Variant().(schema.SessionUpdateAgentMessage)
	if !ok {
		t.Fatalf("expected error agent_message while foreground turn active, got %#v", ev)
	}
	blocks, _ := msg.Content.Get()
	if got := acp2.JoinTexts(blocks); !strings.Contains(got, "rename failed") {
		t.Fatalf("error reply = %q, want mention of rename failed", got)
	}
}

func TestSkillsRefreshErrorPreservesExistingSkills(t *testing.T) {
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 4),
		pending: make(map[uint64]chan message), sessions: make(map[string]*Session),
	}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1", Cwd: "/work"}})
	s.skills = []skillInfo{{Name: "deploy"}}
	s.publishedCmdsKey = commandsKey(codexCommands(s.skills))
	t.Cleanup(func() { s.end(nil) })

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.refreshSkills()
	}()
	var req message
	if err := json.Unmarshal(<-p.writes, &req); err != nil {
		t.Fatal(err)
	}
	p.deliverReply(message{ID: req.ID, Error: &rpcError{Code: -32603, Message: "temporary error"}})
	<-done

	if len(s.skills) != 1 || s.skills[0].Name != "deploy" {
		t.Fatalf("skills wiped on transient RPC error: %+v", s.skills)
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("unexpected event on failed skills refresh: %#v", ev)
	default:
	}
}
