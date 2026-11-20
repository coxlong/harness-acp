package claude

import (
	"strings"
	"testing"
)

// A Monitor as Claude Code 2.1.319 reports it: a local_bash task that is in the
// live set before its task_started and before its call returns.
const (
	monUse     = `{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tm","name":"Monitor","input":{"command":"for i in 1 2 3; do echo tick $i; sleep 2; done","description":"ticks","timeout_ms":30000}}]},"parent_tool_use_id":null}`
	monChanged = `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"mon1","run_id":"r","task_type":"local_bash","description":"ticks"}]}`
	monStarted = `{"type":"system","subtype":"task_started","task_id":"mon1","run_id":"r","tool_use_id":"tm","description":"ticks","is_backgrounded":true,"task_type":"local_bash"}`
	monResult  = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tm","type":"tool_result","content":"Monitor started (task mon1, expires in 30s unless the source ends first)."}]},"parent_tool_use_id":null,"tool_use_result":{"taskId":"mon1","timeoutMs":30000,"persistent":false}}`
	monUpdated = `{"type":"system","subtype":"task_updated","task_id":"mon1","run_id":"r","patch":{"status":"completed","end_time":1}}`
	monNotif   = `{"type":"system","subtype":"task_notification","task_id":"mon1","run_id":"r","tool_use_id":"tm","status":"completed","output_file":"/x/mon1.output","summary":"Monitor \"ticks\" stream ended"}`
)

// A tool call whose task goes on stays in progress, is listed as background
// work that can be cancelled, and ends with its task.
func TestMonitorCallStaysRunning(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, monUse, monChanged, monStarted, monResult)
	wantEvents(t, summarize(drain(s)),
		"tool:tm in_progress",
		"state:1 bg=0",        // the list came before the call had its task
		"tool:tm in_progress", // the result does not complete the call
		"tool:tm  +cancel",    // before the state that lists it
		"state:1 bg=1",
	)

	feed(s, &proc{}, monUpdated, monNotif, bgEmpty)
	wantEvents(t, summarize(drain(s)),
		"tool:tm completed -cancel",
		"state:1 bg=0",
	)
}

// A task that leaves the live set without a notice has ended.
func TestCallEndsWhenItsTaskLeavesTheSet(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, monUse, monChanged, monStarted, monResult)
	drain(s)
	feed(s, &proc{}, bgEmpty)
	if got := strings.Join(summarize(drain(s)), "\n"); !strings.Contains(got, "tool:tm completed -cancel") {
		t.Errorf("events = %s", got)
	}
}

func TestPausedAndStoppedCall(t *testing.T) {
	paused := `{"type":"system","subtype":"task_updated","task_id":"mon1","patch":{"status":"paused"}}`
	resumed := `{"type":"system","subtype":"task_updated","task_id":"mon1","patch":{"status":"running"}}`
	killed := `{"type":"system","subtype":"task_updated","task_id":"mon1","patch":{"status":"killed"}}`
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, monUse, monChanged, monStarted, monResult)
	drain(s)
	feed(s, &proc{}, paused, paused, resumed, killed)
	wantEvents(t, summarize(drain(s)),
		"tool:tm paused",
		"tool:tm in_progress",
		"tool:tm cancelled -cancel",
		"state:1 bg=0", // an ended call is no longer listed
	)
}

// Background work that no tool call shows gets a call of its own, started
// before the list that names it.
func TestAdoptedTask(t *testing.T) {
	changed := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"h1","task_type":"dream","description":"Consolidating memory"},{"task_id":"amb","task_type":"dream","description":"housekeeping","ambient":true}]}`
	started := `{"type":"system","subtype":"task_started","task_id":"h1","description":"Consolidating memory","task_type":"dream"}`
	ended := `{"type":"system","subtype":"task_notification","task_id":"h1","status":"failed","summary":"boom"}`
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, changed, started)
	wantEvents(t, summarize(drain(s)),
		"state:1 bg=0", // not started yet
		"tool:task_h1 in_progress",
		"tool:task_h1  +cancel",
		"state:1 bg=1", // the ambient task is not listed
	)
	if id, ok := s.tr.callTask("parent", "parent", "task_h1"); !ok || id != "h1" {
		t.Errorf("callTask = %q, %v", id, ok)
	}
	if _, ok := s.tr.callTask("parent", "parent", "nope"); ok {
		t.Error("a call that stands for no task was found")
	}
	feed(s, &proc{}, ended, bgEmpty)
	wantEvents(t, summarize(drain(s)),
		"tool:task_h1 failed -cancel",
		"state:1 bg=0",
	)
}

// A task that a tool call started is part of that call until it returns.
func TestTaskIsNotAdoptedBeforeItsCallReturns(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	wfUse := `{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tw","name":"Workflow","input":{}}]},"parent_tool_use_id":null}`
	changed := `{"type":"system","subtype":"background_tasks_changed","tasks":[{"task_id":"wf1","task_type":"local_workflow","description":"Run a probe"}]}`
	started := `{"type":"system","subtype":"task_started","task_id":"wf1","tool_use_id":"tw","description":"Run a probe","task_type":"local_workflow"}`
	progress := `{"type":"system","subtype":"task_progress","task_id":"wf1","description":"Run a probe","summary":"Run a probe","last_tool_name":"probe","usage":{"total_tokens":4003,"tool_uses":2,"duration_ms":1000},"workflow_progress":[{"type":"workflow_phase","index":0,"title":"Count"},{"type":"workflow_agent","label":"two","state":"done","phaseIndex":0,"tokens":14629,"toolCalls":0,"durationMs":1200},{"type":"workflow_agent","label":"three","state":"start","phaseIndex":0,"lastToolName":"Bash","lastToolSummary":"echo 3"}]}`
	result := `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tw","type":"tool_result","content":"Workflow started"}]},"parent_tool_use_id":null,"tool_use_result":{"taskId":"wf1"}}`
	feed(s, &proc{}, wfUse, changed, started, progress)
	got := strings.Join(summarize(drain(s)), "\n")
	if strings.Contains(got, "task_wf1") {
		t.Errorf("adopted a task whose call had not returned: %s", got)
	}
	feed(s, &proc{}, result)
	got = strings.Join(summarize(drain(s)), "\n")
	// The progress that came before the result goes on the call after it.
	if !strings.Contains(got, "tool:tw in_progress\ntool:tw \n") || strings.Contains(got, "task_wf1") {
		t.Errorf("events = %s", got)
	}
}

func TestWorkflowCardText(t *testing.T) {
	p := taskProgress{
		description: "Run a probe", summary: "Run a probe", lastTool: "probe",
		usage: &taskUsage{TotalTokens: 4003, ToolUses: 2, DurationMs: 1999},
		workflow: parseWorkflow([]byte(`[{"type":"workflow_phase","index":0,"title":"Count"},{"type":"workflow_phase","index":1,"title":"Sum"},
			{"type":"workflow_agent","label":"two","state":"done","phaseIndex":0,"tokens":14629,"toolCalls":0,"durationMs":1200,"lastToolName":"Bash"},
			{"type":"workflow_agent","label":"three","state":"start","phaseIndex":0,"attempt":2,"lastToolName":"Bash","lastToolSummary":"echo 3"},
			{"type":"workflow_agent","label":"loose","state":"start"},{"type":"junk"}]`)),
	}
	title, body, ok := workflowText(p)
	if !ok || title != "Workflow: Run a probe" {
		t.Fatalf("title = %q ok=%v", title, ok)
	}
	want := "Run a probe\n\nLast activity: probe\n\n4,003 tokens · 2 tool calls · 1s\n\n" +
		"- loose: running\n\n" +
		"**Count**\n- two: done · 14,629 tokens · 0 tool calls · 1s\n- three: running (attempt 2) · Bash: echo 3\n\n" +
		"**Sum**\n- not started"
	if body != want {
		t.Errorf("body:\n%s\nwant:\n%s", body, want)
	}
	if _, _, ok := workflowText(taskProgress{}); ok {
		t.Error("an empty report made a card")
	}
}

// When the process exits, its background work ends with it.
func TestProcessEndedEndsBackgroundWork(t *testing.T) {
	s := NewSession("parent", nil, SpawnOpts{Cwd: "/work"})
	s.foreground = true
	feed(s, &proc{}, monUse, monChanged, monStarted, monResult)
	drain(s)
	got := summarize(s.tr.processEnded())
	if strings.Join(got, "\n") != "tool:tm cancelled -cancel" {
		t.Errorf("events = %v", got)
	}
	if len(s.tr.backgroundWork()) != 0 {
		t.Error("work is still listed")
	}
}
