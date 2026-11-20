package claude

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// summarize names each event in a compact form: kind, terminal or session, and
// what changed.
func summarize(evs []backend.Event) []string {
	var out []string
	for _, ev := range evs {
		pre := ""
		if ev.Session != "" {
			pre = "[" + ev.Session + "] "
		}
		switch ev.Kind {
		case backend.EventTurnState:
			out = append(out, fmt.Sprintf("%sstate:%d bg=%d", pre, ev.Turn.State, len(ev.Turn.Background)))
		case backend.EventUpdate:
			switch v := ev.Update.Variant().(type) {
			case schema.SessionUpdateTerminalUpdate:
				d := "terminal:" + string(v.TerminalID)
				if c, ok := v.Command.Get(); ok {
					d += " cmd=" + c
				}
				if o, ok := v.Output.Get(); ok {
					raw, _ := base64.StdEncoding.DecodeString(o.Data)
					d += fmt.Sprintf(" out=%q", raw)
				}
				if c, ok := v.Capabilities.Get(); ok && c.Cancel != nil {
					d += " +cancel"
				} else if v.Capabilities.IsNull() {
					d += " -cancel"
				}
				if e, ok := v.ExitStatus.Get(); ok {
					if e.ExitCode != nil {
						d += fmt.Sprintf(" exit=%d", *e.ExitCode)
					} else {
						d += " exit=?"
					}
				}
				out = append(out, pre+d)
			case schema.SessionUpdateToolCallUpdate:
				st, _ := v.Status.Get()
				d := fmt.Sprintf("%stool:%s %s", pre, v.ToolCallID, st)
				if c, ok := v.Capabilities.Get(); ok && c.Cancel != nil {
					d += " +cancel"
				} else if v.Capabilities.IsNull() {
					d += " -cancel"
				}
				out = append(out, d)
			case schema.SessionUpdateSubagentUpdate:
				d := "subagent:" + string(v.SessionID)
				if st, ok := v.State.Get(); ok {
					d += " " + st.Tag()
					if idle, ok := st.As[schema.StateUpdateIdle](); ok {
						d += "/" + idle.Value.Tag()
					}
				}
				out = append(out, pre+d)
			case schema.SessionUpdateSessionMessage:
				txt := ""
				if c, ok := v.Content.Get(); ok {
					txt, _ = acp2.TextOf(c[0])
				}
				out = append(out, fmt.Sprintf("%smessage:%s %q", pre, v.MessageID, txt))
			case schema.SessionUpdateAgentMessageChunk:
				txt, _ := acp2.TextOf(v.Content)
				out = append(out, fmt.Sprintf("%sagent:%q", pre, txt))
			case schema.SessionUpdateTerminalOutputChunk:
				raw, _ := base64.StdEncoding.DecodeString(v.Data)
				out = append(out, fmt.Sprintf("%schunk:%s %q", pre, v.TerminalID, raw))
			}
		}
	}
	return out
}

func run(t *testing.T, lines ...string) []string {
	t.Helper()
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, lines...)
	return summarize(drain(s))
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const (
	bashUse    = `{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tb","name":"Bash","input":{"command":"echo hi","description":"Print"}}]},"parent_tool_use_id":null}`
	bashOK     = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tb","type":"tool_result","content":"hi","is_error":false}]},"parent_tool_use_id":null,"tool_use_result":{"stdout":"hi","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false}}`
	bashExit2  = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tb","type":"tool_result","content":"Exit code 2\nboom","is_error":true}]},"parent_tool_use_id":null,"tool_use_result":"Error: Exit code 2\nboom"}`
	bgBashUse  = `{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tb","name":"Bash","input":{"command":"sleep 4; echo bg-done","run_in_background":true}}]},"parent_tool_use_id":null}`
	bgChanged  = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"bt1","run_id":"r","task_type":"local_bash","description":"Sleep"}]}`
	bgStarted  = `{"type":"system","subtype":"task_started","task_id":"bt1","run_id":"r","tool_use_id":"tb","description":"Sleep","is_backgrounded":true,"task_type":"local_bash"}`
	bgResult   = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tb","type":"tool_result","content":"Command running in background with ID: bt1. Output is being written to: /nonexistent/bt1.output. You will be notified when it completes.","is_error":false}]},"parent_tool_use_id":null,"tool_use_result":{"stdout":"","stderr":"","interrupted":false,"backgroundTaskId":"bt1"}}`
	bgUpdated  = `{"type":"system","subtype":"task_updated","task_id":"bt1","run_id":"r","patch":{"status":"completed","end_time":1}}`
	bgNotified = `{"type":"system","subtype":"task_notification","task_id":"bt1","run_id":"r","tool_use_id":"tb","status":"completed","summary":"Background command \"Sleep\" completed (exit code 0)"}`
	bgEmpty    = `{"type":"system","subtype":"background_tasks_changed","tasks":[]}`
)

func TestForegroundBashHasTerminal(t *testing.T) {
	wantEvents(t, run(t, bashUse, bashOK),
		"terminal:tb",
		"terminal:tb cmd=echo hi",
		"tool:tb in_progress",
		`terminal:tb out="hi"`,
		"terminal:tb exit=0",
		"tool:tb completed",
	)
	got := run(t, bashUse, bashExit2)
	wantEvents(t, got,
		"terminal:tb",
		"terminal:tb cmd=echo hi",
		"tool:tb in_progress",
		`terminal:tb out="Exit code 2\r\nboom"`,
		"terminal:tb exit=2",
		"tool:tb failed",
	)
}

// A background command keeps its terminal running, and is listed as background
// work until it ends. Claude Code's real order is: the list (before
// task_started), task_started, the Bash result, then the end signals.
func TestBackgroundBashLifecycle(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, bgBashUse, bgChanged, bgStarted, bgResult)
	got := summarize(drain(s))
	wantEvents(t, got,
		"terminal:tb",
		"terminal:tb cmd=sleep 4; echo bg-done",
		"tool:tb in_progress",
		"state:1 bg=0", // the list arrives before the command is known as a terminal
		`terminal:tb out="Command running in background with ID: bt1. Output is being written to: /nonexistent/bt1.output. You will be notified when it completes."`,
		"tool:tb completed",
		"terminal:tb +cancel", // before the state that lists it
		"state:1 bg=1",
	)
	if len(s.tailers) != 1 {
		t.Fatalf("%d tails, want one for the output file", len(s.tailers))
	}

	// Both end signals arrive; the terminal exits once, then leaves the list.
	feed(s, &proc{}, bgUpdated, bgNotified, bgEmpty)
	deadline := time.After(3 * time.Second)
	var evs []string
	for !strings.Contains(strings.Join(evs, "\n"), "exit=0") {
		select {
		case <-deadline:
			t.Fatalf("no exit: %v", evs)
		case <-time.After(20 * time.Millisecond):
			evs = append(evs, summarize(drain(s))...)
		}
	}
	feed(s, &proc{}, bgNotified)
	evs = append(evs, summarize(drain(s))...)
	exits := 0
	for _, e := range evs {
		if strings.Contains(e, "exit=") {
			exits++
		}
	}
	if exits != 1 {
		t.Errorf("%d exits, want 1: %v", exits, evs)
	}
	if !contains(strings.Join(evs, "\n"), "exit=0") {
		t.Errorf("the exit code of the summary was lost: %v", evs)
	}
	if !contains(strings.Join(evs, "\n"), "state:1 bg=0") {
		t.Errorf("the list never emptied: %v", evs)
	}
}

func TestTailFollowsOutputFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.output")
	s := NewSession("parent", nil, SpawnOpts{})
	s.foreground = true
	s.mu.Lock()
	s.startTail(tailJob{terminal: "tb", path: path})
	s.mu.Unlock()

	write := func(text string) {
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		f.WriteString(text)
		f.Close()
	}
	collect := func(n int) []string {
		var got []string
		deadline := time.After(5 * time.Second)
		for len(got) < n {
			select {
			case <-deadline:
				t.Fatalf("got %v, want %d events", got, n)
			case <-time.After(30 * time.Millisecond):
				got = append(got, summarize(drain(s))...)
			}
		}
		return got
	}
	write("one\n")
	wantEvents(t, collect(1), `terminal:tb out="one\r\n"`) // the first read is a snapshot
	write("two\n")
	wantEvents(t, collect(1), `chunk:tb "two\r\n"`)
	write("three\n")
	s.mu.Lock()
	s.stopTerminal(stopJob{terminal: "tb"})
	s.mu.Unlock()
	wantEvents(t, collect(2), `chunk:tb "three\r\n"`, "terminal:tb exit=?") // flushed before the exit
}

// A foreground subagent, as Claude Code 2.1.319 printed it.
const (
	agentUse     = `{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"ta","name":"Agent","input":{"description":"Answer 2+2","subagent_type":"general-purpose","prompt":"What is 2+2?","run_in_background":false}}]},"parent_tool_use_id":null}`
	agentStarted = `{"type":"system","subtype":"task_started","task_id":"ag1","run_id":"r","tool_use_id":"ta","description":"Answer 2+2","subagent_type":"general-purpose","is_backgrounded":false,"task_type":"local_agent","prompt":"What is 2+2?"}`
	childPrompt  = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"What is 2+2?"}]},"parent_tool_use_id":"ta","agent_id":"ag1"}`
	childText    = `{"type":"assistant","message":{"id":"cm","model":"claude-sonnet-5-5","content":[{"type":"text","text":"4"}]},"parent_tool_use_id":"ta"}`
	agentUpdated = `{"type":"system","subtype":"task_updated","task_id":"ag1","run_id":"r","patch":{"status":"completed"}}`
	agentNotif   = `{"type":"system","subtype":"task_notification","task_id":"ag1","run_id":"r","tool_use_id":"ta","status":"completed","summary":"4"}`
	agentResult  = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"ta","type":"tool_result","content":[{"type":"text","text":"[Subagent hand-back] The text below is the final report of a subagent. The report follows:\n  4\nagentId: ag1 (use SendMessage)\n<usage>subagent_tokens: 1</usage>"}]}]},"parent_tool_use_id":null}`
)

func TestForegroundSubagent(t *testing.T) {
	wantEvents(t, run(t, agentUse, agentStarted, childPrompt, childText, agentUpdated, agentNotif, agentResult),
		"subagent:ag1 running",
		"[ag1] state:1 bg=0",
		`message:ta "What is 2+2?"`,
		`[ag1] message:ta "What is 2+2?"`,
		`[ag1] agent:"4"`,
		"subagent:ag1 idle/end_turn",
		"[ag1] state:0 bg=0",
		`message:ta:reply "4"`,
	)
}

// What the child said before it was announced is delivered after the announcement.
func TestSubagentLinesBeforeAnnounce(t *testing.T) {
	got := run(t, agentUse, childText, agentStarted)
	i, j := indexOf(got, `[ag1] agent:"4"`), indexOf(got, "subagent:ag1 running")
	if i < 0 || j < 0 || i < j {
		t.Errorf("events = %v", got)
	}
}

// A background subagent: the launch result says nothing, and the child is background work.
func TestBackgroundSubagent(t *testing.T) {
	started := strings.Replace(agentStarted, `"is_backgrounded":false`, `"is_backgrounded":true`, 1)
	changed := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"ag1","run_id":"r","task_type":"local_agent"}]}`
	launched := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"ta","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"parent_tool_use_id":null}`
	got := run(t, agentUse, changed, started, launched)
	for _, e := range got {
		if strings.HasPrefix(e, "tool:ta") || strings.HasPrefix(e, "message:ta:reply") {
			t.Errorf("the launch showed as %q", e)
		}
	}
	if !contains(strings.Join(got, "\n"), "state:1 bg=1") {
		t.Errorf("the subagent is not background work: %v", got)
	}
}

// An Agent call that no task came of is shown as a tool call.
func TestAgentCallWithoutSubagent(t *testing.T) {
	failed := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"ta","type":"tool_result","content":"Unknown agent type","is_error":true}]},"parent_tool_use_id":null}`
	wantEvents(t, run(t, agentUse, failed), "tool:ta failed")
}

func TestHandbackText(t *testing.T) {
	in := "[Subagent hand-back] frame text. The report follows:\n  line one\n  line two\nagentId: x (use SendMessage)\n<usage>subagent_tokens: 1</usage>"
	if got := handbackText(in); got != "line one\nline two" {
		t.Errorf("handbackText = %q", got)
	}
	if got := handbackText("plain"); got != "plain" {
		t.Errorf("plain = %q", got)
	}
}

func indexOf(s []string, v string) int {
	for i, e := range s {
		if e == v {
			return i
		}
	}
	return -1
}

func TestCancelChildKnowsItsChildren(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{})
	s.foreground = true
	feed(s, &proc{}, agentUse, agentStarted)
	if found, _ := s.CancelChild(nil, "someone-else"); found {
		t.Error("a session that is not a child was claimed")
	}
	// An ended subagent keeps its outcome: nothing to stop, and no process needed.
	feed(s, &proc{}, agentUpdated, agentNotif)
	if found, err := s.CancelChild(nil, "ag1"); !found || err != nil {
		t.Errorf("ended child: found=%v err=%v", found, err)
	}
}

// A subagent that was stopped ends cancelled, on both sides.
func TestStoppedSubagentEndsCancelled(t *testing.T) {
	killed := `{"type":"system","subtype":"task_updated","task_id":"ag1","patch":{"status":"killed"}}`
	stopped := `{"type":"system","subtype":"task_notification","task_id":"ag1","tool_use_id":"ta","status":"stopped","summary":"stopped"}`
	got := run(t, agentUse, agentStarted, killed, stopped)
	if !contains(strings.Join(got, "\n"), "subagent:ag1 idle/cancelled") {
		t.Errorf("events = %v", got)
	}
}

// What a subagent's own background command looks like in Claude Code 2.1.319:
// a task_started that names the subagent as its parent, and the Bash result in
// the subagent's own stream.
const (
	childBashUse     = `{"type":"assistant","message":{"id":"cm2","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"cb","name":"Bash","input":{"command":"sleep 40"}}]},"parent_tool_use_id":"ta"}`
	childBashStarted = `{"type":"system","subtype":"task_started","task_id":"cbt","tool_use_id":"cb","description":"sleep 40","is_backgrounded":true,"parent_task_id":"ag1","owned_by_subagent":true,"task_type":"local_bash"}`
	childBashResult  = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"cb","type":"tool_result","content":"Command running in background with ID: cbt. Output is being written to: /nonexistent/cbt.output. You will be notified when it completes."}]},"parent_tool_use_id":"ta","tool_use_result":{"stdout":"","backgroundTaskId":"cbt"}}`
	childBgChanged   = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"ag1","task_type":"local_agent"},{"task_id":"cbt","task_type":"local_bash"}]}`
	childBashNotif   = `{"type":"system","subtype":"task_notification","task_id":"cbt","tool_use_id":"cb","status":"completed","summary":"Background command completed (exit code 0)"}`
)

func TestSubagentBackgroundCommand(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{})
	s.foreground = true
	feed(s, &proc{}, agentUse, agentStarted, childBashUse, childBashStarted, childBgChanged, childBashResult)
	got := summarize(drain(s))
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"[ag1] terminal:cb",                      // the terminal is in the subagent's session
		`[ag1] terminal:cb out="Command running`, // with the launch notice
		"[ag1] terminal:cb +cancel",              // and can be cancelled
		"[ag1] state:1 bg=1",                     // the subagent's own list names it
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in\n%s", want, joined)
		}
	}
	if len(s.tailers) != 1 {
		t.Fatalf("%d tails, want the subagent's", len(s.tailers))
	}
	if id, ok := s.tr.terminalTask("ag1", "parent", "cb"); !ok || id != "cbt" {
		t.Errorf("terminalTask = %q %v", id, ok)
	}
	if _, ok := s.tr.terminalTask("parent", "parent", "cb"); ok {
		t.Error("the subagent's terminal was found in the parent session")
	}

	feed(s, &proc{}, childBashNotif)
	deadline := time.After(3 * time.Second)
	var evs []string
	for !strings.Contains(strings.Join(evs, "\n"), "[ag1] terminal:cb -cancel exit=0") {
		select {
		case <-deadline:
			t.Fatalf("no exit in the subagent's session: %v", evs)
		case <-time.After(20 * time.Millisecond):
			evs = append(evs, summarize(drain(s))...)
		}
	}
}

// A subagent's permission request is asked in its session, and the subagent
// waits for the user meanwhile.
func TestSubagentPermission(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{})
	s.foreground = true
	feed(s, &proc{}, agentUse, agentStarted)
	drain(s)
	feed(s, &proc{}, `{"type":"control_request","request_id":"rq","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"/x","content":"hi"},"tool_use_id":"cw","agent_id":"ag1"}}`)
	var asked *backend.PermissionRequest
	var states []string
	for _, ev := range drain(s) {
		if ev.Kind == backend.EventPermission {
			p := ev.Permission
			asked = &p
		}
		if ev.Session == "ag1" && ev.Kind == backend.EventTurnState {
			states = append(states, fmt.Sprint(ev.Turn.State))
		}
	}
	if asked == nil || asked.Session != "ag1" {
		t.Fatalf("request = %+v, want it in the subagent's session", asked)
	}
	if strings.Join(states, ",") != "2" {
		t.Errorf("subagent states = %v, want requires_action", states)
	}

	// The answer, a refusal, puts the subagent back to running.
	p := &proc{stdin: nopWriteCloser{io.Discard}, done: make(chan struct{})}
	s.pending["rq"] = pendingPerm{p: p, input: nil, child: "ag1"}
	if err := s.Respond("rq", backend.Decision{}); err != nil {
		t.Fatal(err)
	}
	states = nil
	for _, ev := range drain(s) {
		if ev.Session == "ag1" && ev.Kind == backend.EventTurnState {
			states = append(states, fmt.Sprint(ev.Turn.State))
		}
	}
	if strings.Join(states, ",") != "1" {
		t.Errorf("subagent states after the answer = %v, want running", states)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// A message to a finished subagent, as Claude Code 2.1.319 printed it: the same
// task starts again under the SendMessage call's id, the subagent's messages
// still carry the call that launched it, and the SendMessage result names the
// agent it resumed.
const (
	smUse     = `{"type":"assistant","message":{"id":"m2","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tsm","name":"SendMessage","input":{"to":"ag1","message":"What number?","summary":"ask","type":"message"}}]},"parent_tool_use_id":null}`
	smStarted = `{"type":"system","subtype":"task_started","task_id":"ag1","tool_use_id":"tsm","is_backgrounded":true,"task_type":"local_agent"}`
	smResult  = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tsm","type":"tool_result","content":[{"type":"text","text":"{\"success\":true,\"message\":\"Resuming agent ag1\",\"resumedAgentId\":\"ag1\"}"}]}]},"parent_tool_use_id":null,"tool_use_result":{"success":true,"message":"Resuming agent ag1","resumedAgentId":"ag1","revivedAgentId":"ag1"}}`
	smText    = `{"type":"assistant","message":{"id":"cm2","model":"claude-sonnet-5-5","content":[{"type":"text","text":"7"}]},"parent_tool_use_id":"ta"}`
	smNotif   = `{"type":"system","subtype":"task_notification","task_id":"ag1","tool_use_id":"tsm","status":"completed","summary":"7"}`
	oldNotif  = `{"type":"system","subtype":"task_notification","task_id":"ag1","run_id":"r","tool_use_id":"ta","status":"completed","summary":"late"}`
)

func TestSendMessageResumesSubagent(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, agentUse, agentStarted, childText, agentUpdated, agentNotif, agentResult)
	drain(s)
	feed(s, &proc{}, smUse, smStarted)
	got := summarize(drain(s))
	wantEvents(t, got,
		"subagent:ag1 running", // the same child, running again
		"[ag1] state:1 bg=0",
		`message:tsm "What number?"`, // the SendMessage call is the message
		`[ag1] message:tsm "What number?"`,
	)
	feed(s, &proc{}, smResult, smText, oldNotif)
	got = summarize(drain(s))
	// The result shows no tool call, the answer goes to the child, and the end
	// of the earlier run ends nothing.
	wantEvents(t, got, `[ag1] agent:"7"`)
	feed(s, &proc{}, agentUpdated, smNotif)
	got = summarize(drain(s))
	wantEvents(t, got, "subagent:ag1 idle/end_turn", "[ag1] state:0 bg=0")
}

// A message that reaches no subagent is shown as the tool call it is, once it ends.
func TestSendMessageToNobodyIsAToolCall(t *testing.T) {
	use := strings.Replace(smUse, `"to":"ag1"`, `"to":"parent"`, 1)
	result := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tsm","type":"tool_result","content":"sent"}]},"parent_tool_use_id":null,"tool_use_result":{"success":true}}`
	wantEvents(t, run(t, use, result), "tool:tsm in_progress", "tool:tsm completed")
}

// Claude Code can resume a subagent without a task_started: the result says so.
func TestSendMessageResultResumes(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, agentUse, agentStarted, agentUpdated, agentNotif, agentResult)
	drain(s)
	feed(s, &proc{}, smUse, smResult)
	wantEvents(t, summarize(drain(s)),
		"subagent:ag1 running", "[ag1] state:1 bg=0",
		`message:tsm "What number?"`, `[ag1] message:tsm "What number?"`)
}

// A call that stops or messages a subagent refers to its session.
func TestTaskStopReferencesTheChild(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, agentUse, agentStarted)
	drain(s)
	stop := `{"type":"assistant","message":{"id":"m3","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tst","name":"TaskStop","input":{"task_id":"ag1"}}]},"parent_tool_use_id":null}`
	var refs []string
	for _, ev := range func() []backend.Event { feed(s, &proc{}, stop); return drain(s) }() {
		if u, ok := ev.Update.Variant().(schema.SessionUpdateToolCallUpdate); ok {
			if c, ok := u.Content.Get(); ok {
				for _, x := range c {
					if r, ok := x.As[schema.ToolCallContentSession](); ok {
						refs = append(refs, string(r.SessionID))
					}
				}
			}
		}
	}
	if len(refs) != 1 || refs[0] != "ag1" {
		t.Errorf("session references = %v", refs)
	}
}
