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
	case schema.SessionUpdateUsageUpdate:
		return "usage"
	case schema.SessionUpdateSessionInfoUpdate:
		return "session_info"
	case schema.SessionUpdatePlanUpdate:
		return "plan"
	case schema.SessionUpdateConfigOptionUpdate:
		return "config_options"
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
	for _, hu := range hist.Updates {
		u := hu.Update
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
		t.Errorf("history user=%v agent=%v (%d updates)", sawUser, sawAgent, len(hist.Updates))
	}

	list, _, err := b.ListSessions(ctx, backend.ListFilter{Cwd: dir, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, si := range list {
		if si.ID == id {
			found = true
			// The first prompt, or the generated title that replaces it.
			if !strings.Contains(strings.ToLower(si.Title), "pong") {
				t.Errorf("title = %q, want the first prompt or its generated title", si.Title)
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

// A session reports its options at once. A turn with tool uses shows a diff for
// the Write, the context usage with its cost, and the session's title.
func TestE2E_OptionsDiffUsageTitle(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)

	opts := s.Options()
	ids := map[string]bool{}
	for _, o := range opts.Config {
		if sel, ok := o.As[schema.SessionConfigOptionSelect](); ok {
			ids[string(sel.ConfigID)] = true
		}
	}
	if !ids["mode"] || !ids["model"] {
		t.Errorf("config options = %v, want mode and model", ids)
	}
	if len(opts.Commands) == 0 {
		t.Error("no commands")
	}

	target := filepath.Join(dir, "x.txt")
	if _, err := s.Send(ctx, text("Use the Write tool to create "+target+" with the two lines a and b. Then reply done.")); err != nil {
		t.Fatal(err)
	}
	var got []backend.Event
	for {
		evs := until(t, s, func(ev backend.Event) bool { return isIdle(ev) || permission(ev) })
		got = append(got, evs...)
		if last := evs[len(evs)-1]; last.Kind == backend.EventPermission {
			if err := s.Respond(last.Permission.ReqID, backend.Decision{Allow: true}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		break
	}
	t.Log(describe(got))

	var sawDiff, sawUsage, sawCost bool
	for _, ev := range got {
		if ev.Kind != backend.EventUpdate {
			continue
		}
		switch v := ev.Update.Variant().(type) {
		case schema.SessionUpdateToolCallUpdate:
			if c, ok := v.Content.Get(); ok {
				for _, tc := range c {
					if d, ok := tc.As[schema.ToolCallContentDiff](); ok {
						sawDiff = d.Patch != nil && strings.Contains(d.Patch.Text, "+a")
					}
				}
			}
		case schema.SessionUpdateUsageUpdate:
			if v.Used > 0 && v.Size > 0 {
				sawUsage = true
			}
			if _, ok := v.Cost.Get(); ok {
				sawCost = true
			}
		}
	}
	if !sawDiff || !sawUsage || !sawCost {
		t.Errorf("diff=%v usage=%v cost=%v", sawDiff, sawUsage, sawCost)
	}

	// The title comes a few seconds into the turn, or just after it.
	titled := false
	for _, ev := range got {
		titled = titled || kindOfEv(ev) == "session_info"
	}
	if !titled {
		until(t, s, func(ev backend.Event) bool { return kindOfEv(ev) == "session_info" })
	}
}

func permission(ev backend.Event) bool { return ev.Kind == backend.EventPermission }

// A background command shows as a terminal that outlives the turn, and a
// subagent as a child session; both end with the work.
func TestE2E_BackgroundAndSubagent(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)

	prompt := "Do these in order. 1) Run the Bash tool with run_in_background=true and command `sleep 3; echo bg-done`. " +
		"2) Use the Task tool with subagent_type general-purpose to answer: what is 2+2? (foreground). 3) Reply: finished. Do not poll the background task."
	if _, err := s.Send(ctx, text(prompt)); err != nil {
		t.Fatal(err)
	}
	var all []backend.Event
	idles := 0
	for idles < 1 || !sawExit(all) {
		evs := until(t, s, func(ev backend.Event) bool { return isIdle(ev) || permission(ev) })
		all = append(all, evs...)
		if last := evs[len(evs)-1]; last.Kind == backend.EventPermission {
			_ = s.Respond(last.Permission.ReqID, backend.Decision{Allow: true})
		} else {
			idles++
		}
		if idles > 3 {
			break
		}
	}
	got := summarize(all)
	for _, e := range got {
		t.Log(e)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"out=\"Command running in background", "exit=", "subagent:", "message:", ":reply"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in the events", want)
		}
	}
}

func sawExit(evs []backend.Event) bool {
	for _, e := range summarize(evs) {
		if strings.HasPrefix(e, "terminal:") && strings.Contains(e, "exit=") {
			return true
		}
	}
	return false
}

// session/cancel of a subagent's child session stops that subagent, and the
// parent goes on.
func TestE2E_CancelSubagent(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	cs := s.(*Session)

	if _, err := s.Send(ctx, text("Use the Task tool (general-purpose) with run_in_background=true and prompt: Run the Bash tool with command `sleep 40`, then say done. After launching, reply launched and stop.")); err != nil {
		t.Fatal(err)
	}
	var all []backend.Event
	child := ""
	for child == "" {
		ev := until(t, s, func(ev backend.Event) bool { return ev.Session != "" || permission(ev) })
		all = append(all, ev...)
		last := ev[len(ev)-1]
		if last.Kind == backend.EventPermission {
			_ = s.Respond(last.Permission.ReqID, backend.Decision{Allow: true})
		} else {
			child = last.Session
		}
	}
	time.Sleep(3 * time.Second) // let it start working
	found, err := cs.CancelChild(ctx, child)
	if err != nil || !found {
		t.Fatalf("CancelChild = %v, %v", found, err)
	}
	more := until(t, s, func(ev backend.Event) bool {
		return ev.Session == child && ev.Kind == backend.EventTurnState && ev.Turn.State == backend.TurnIdle
	})
	last := more[len(more)-1]
	if last.Turn.StopReason != acp2.StopReasonCancelled {
		t.Errorf("child idle = %q err=%v, want cancelled", last.Turn.StopReason, last.Turn.Err)
	}
}

// terminal/cancel stops a background command: its terminal exits.
func TestE2E_CancelTerminal(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	cs := s.(*Session)

	if _, err := s.Send(ctx, text("Run the Bash tool with run_in_background=true and command `sleep 60`. Then reply launched and stop.")); err != nil {
		t.Fatal(err)
	}
	var term string
	for term == "" {
		evs := until(t, s, func(ev backend.Event) bool { return isIdle(ev) || permission(ev) })
		for _, ev := range evs {
			if ev.Kind == backend.EventPermission {
				_ = s.Respond(ev.Permission.ReqID, backend.Decision{Allow: true})
			}
			if ev.Kind == backend.EventUpdate {
				if u, ok := ev.Update.Variant().(schema.SessionUpdateTerminalUpdate); ok {
					if c, ok := u.Capabilities.Get(); ok && c.Cancel != nil {
						term = string(u.TerminalID)
					}
				}
			}
		}
		if term == "" && isIdle(evs[len(evs)-1]) {
			t.Fatal("the turn ended without a cancellable terminal")
		}
	}
	found, err := cs.CancelTerminal(ctx, cs.uuid, term)
	if err != nil || !found {
		t.Fatalf("CancelTerminal = %v, %v", found, err)
	}
	exited := false
	until(t, s, func(ev backend.Event) bool {
		if ev.Kind == backend.EventUpdate {
			if u, ok := ev.Update.Variant().(schema.SessionUpdateTerminalUpdate); ok && u.TerminalID == schema.TerminalID(term) {
				_, exited = u.ExitStatus.Get()
			}
		}
		return exited
	})
}

// A message to a finished subagent runs the same child session again and is
// shown as a message, not a tool call; the session's history then replays the
// subagent as a child session.
func TestE2E_SendMessageAndReplay(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	id := s.NativeID()
	if _, err := s.Send(ctx, text("Step 1: use the Agent tool (general-purpose, foreground) with prompt 'Remember the number 7 and reply with just: ok'. "+
		"Step 2: load SendMessage with ToolSearch, then use SendMessage to send 'What number did I ask you to remember? Reply with just the number.' to that agent, using the agent id from step 1 as `to`. "+
		"Step 3: reply with the number.")); err != nil {
		t.Fatal(err)
	}
	var all []backend.Event
	for i := 0; i < 6; i++ {
		evs := until(t, s, func(ev backend.Event) bool { return isIdle(ev) || permission(ev) })
		all = append(all, evs...)
		if last := evs[len(evs)-1]; last.Kind == backend.EventPermission {
			_ = s.Respond(last.Permission.ReqID, backend.Decision{Allow: true})
			continue
		}
		if sawChildRunning(all, 2) {
			break
		}
	}
	got := strings.Join(summarize(all), "\n")
	t.Log(got)
	if !strings.Contains(got, "message:") || strings.Count(got, "subagent:") < 2 {
		t.Errorf("the message to the subagent did not run its session again")
	}
	if strings.Contains(got, "SendMessage") {
		t.Errorf("a SendMessage call was shown")
	}
	_ = s.Close(ctx)

	hist, err := b.History(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	children, child := 0, 0
	for _, u := range hist.Updates {
		if u.Session != "" {
			child++
		}
	}
	children = len(hist.Children)
	if children != 1 || child == 0 || hist.Missing != 0 {
		t.Errorf("replay: %d children, %d child updates, %d missing", children, child, hist.Missing)
	}
}

func sawChildRunning(evs []backend.Event, n int) bool {
	c := 0
	for _, e := range summarize(evs) {
		if strings.HasPrefix(e, "subagent:") && strings.HasSuffix(e, " running") {
			c++
		}
	}
	return c >= n
}

// A Monitor's tool call stays in progress while its task runs, can be
// cancelled with tool_call/cancel, and ends cancelled.
func TestE2E_CancelMonitorCall(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	cs := s.(*Session)

	if _, err := s.Send(ctx, text("Use the Monitor tool (load it with ToolSearch first) on the command `for i in $(seq 1 60); do echo tick $i; sleep 2; done` with a long timeout. Then reply launched and stop.")); err != nil {
		t.Fatal(err)
	}
	var call string
	var all []backend.Event
	for call == "" {
		evs := until(t, s, func(ev backend.Event) bool { return isIdle(ev) || permission(ev) })
		all = append(all, evs...)
		for _, ev := range evs {
			if ev.Kind == backend.EventPermission {
				_ = s.Respond(ev.Permission.ReqID, backend.Decision{Allow: true})
			}
			if ev.Kind == backend.EventUpdate {
				if u, ok := ev.Update.Variant().(schema.SessionUpdateToolCallUpdate); ok {
					if c, ok := u.Capabilities.Get(); ok && c.Cancel != nil {
						call = string(u.ToolCallID)
					}
				}
			}
		}
		if call == "" && isIdle(evs[len(evs)-1]) {
			t.Fatalf("the turn ended without a cancellable tool call: %v", summarize(all))
		}
	}
	// While the task runs, the call has not ended.
	for _, e := range summarize(all) {
		if strings.HasPrefix(e, "tool:"+call+" completed") {
			t.Errorf("the call ended while its task runs: %v", summarize(all))
		}
	}
	found, err := cs.CancelToolCall(ctx, cs.uuid, call)
	if err != nil || !found {
		t.Fatalf("CancelToolCall = %v, %v", found, err)
	}
	ended := false
	until(t, s, func(ev backend.Event) bool {
		if ev.Kind == backend.EventUpdate {
			if u, ok := ev.Update.Variant().(schema.SessionUpdateToolCallUpdate); ok && string(u.ToolCallID) == call {
				if st, ok := u.Status.Get(); ok && st == schema.ToolCallStatusCancelled {
					ended = true
				}
			}
		}
		return ended
	})
}

// A subagent's permission request is asked in its own session, and its own
// background command has a terminal there.
func TestE2E_SubagentPermissionAndCommand(t *testing.T) {
	b, dir := e2e(t)
	ctx := context.Background()
	s, err := b.NewSession(ctx, backend.NewSessionParams{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)

	target := filepath.Join(dir, "x.txt")
	if _, err := s.Send(ctx, text("Use the Task tool (general-purpose), foreground, with prompt: Use the Write tool to create "+target+" containing hi, then run the Bash tool with run_in_background=true and command `sleep 2; echo child-bg`, then reply done. After it returns, reply finished.")); err != nil {
		t.Fatal(err)
	}
	var all []backend.Event
	asked := ""
	for i := 0; i < 6; i++ {
		evs := until(t, s, func(ev backend.Event) bool { return isIdle(ev) || permission(ev) })
		all = append(all, evs...)
		last := evs[len(evs)-1]
		if last.Kind == backend.EventPermission {
			asked = last.Permission.Session
			_ = s.Respond(last.Permission.ReqID, backend.Decision{Allow: true})
			continue
		}
		break
	}
	for _, e := range summarize(all) {
		t.Log(e)
	}
	if asked == "" {
		t.Error("the subagent's permission request was asked in the parent session")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("file not written: %v", err)
	}
}
