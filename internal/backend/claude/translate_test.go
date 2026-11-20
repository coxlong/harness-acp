package claude

import (
	"strings"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Lines shaped as Claude Code 2.1.319 printed them in a real run.
const (
	writeUse    = `{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"tool_use","id":"tw","name":"Write","input":{"file_path":"/tmp/p/f.txt","content":"a\nb\n"}}],"usage":{"input_tokens":2,"cache_creation_input_tokens":16295,"cache_read_input_tokens":0,"output_tokens":8}},"parent_tool_use_id":null}`
	writeResult = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"tw","type":"tool_result","content":"File created successfully at: /tmp/p/f.txt"}]},"parent_tool_use_id":null,"tool_use_result":{"type":"create","filePath":"/tmp/p/f.txt","content":"a\nb\n","structuredPatch":[],"originalFile":null,"userModified":false}}`
	editUse     = `{"type":"assistant","message":{"id":"m2","model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"tool_use","id":"te","name":"Edit","input":{"file_path":"/tmp/p/f.txt","old_string":"b","new_string":"c"}}]},"parent_tool_use_id":null}`
	editResult  = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"te","type":"tool_result","content":"The file /tmp/p/f.txt has been updated successfully."}]},"parent_tool_use_id":null,"tool_use_result":{"filePath":"/tmp/p/f.txt","oldString":"b","newString":"c","originalFile":"a\nb\n","structuredPatch":[{"oldStart":1,"oldLines":2,"newStart":1,"newLines":2,"lines":[" a","-b","+c"]}],"userModified":false,"replaceAll":false}}`
	readUse     = `{"type":"assistant","message":{"id":"m3","model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"tool_use","id":"tb","name":"Read","input":{"file_path":"/nope"}}]},"parent_tool_use_id":null}`
	readFail    = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tb","content":"boom","is_error":true}]},"parent_tool_use_id":null,"tool_use_result":"Error: boom"}`
	resultOK    = `{"type":"result","subtype":"success","is_error":false,"result":"done","stop_reason":"end_turn","terminal_reason":"completed","num_turns":1,"total_cost_usd":0.05,"modelUsage":{"claude-sonnet-5-5[1m]":{"contextWindow":1000000}}}`
)

// updates runs lines through a session and returns the content updates.
func updates(t *testing.T, lines ...string) []schema.SessionUpdateVariant {
	t.Helper()
	s := NewSession("id", nil, SpawnOpts{})
	s.foreground = true
	feed(s, &proc{}, lines...)
	var out []schema.SessionUpdateVariant
	for _, ev := range drain(s) {
		if ev.Kind == backend.EventUpdate {
			out = append(out, ev.Update.Variant())
		}
	}
	return out
}

func toolUpdate(t *testing.T, us []schema.SessionUpdateVariant, id string, withContent bool) schema.SessionUpdateToolCallUpdate {
	t.Helper()
	for i := len(us) - 1; i >= 0; i-- {
		if u, ok := us[i].(schema.SessionUpdateToolCallUpdate); ok && string(u.ToolCallID) == id {
			if _, has := u.Content.Get(); has == withContent {
				return u
			}
		}
	}
	t.Fatalf("no tool_call_update for %s (content=%v) in %d updates", id, withContent, len(us))
	return schema.SessionUpdateToolCallUpdate{}
}

func diffOf(t *testing.T, u schema.SessionUpdateToolCallUpdate) schema.ToolCallContentDiff {
	t.Helper()
	c, _ := u.Content.Get()
	if len(c) != 1 {
		t.Fatalf("content = %d items", len(c))
	}
	d, ok := c[0].As[schema.ToolCallContentDiff]()
	if !ok {
		t.Fatalf("content is %T, want a diff", c[0].Variant())
	}
	return d
}

func TestWriteResultIsCreateDiff(t *testing.T) {
	d := diffOf(t, toolUpdate(t, updates(t, writeUse, writeResult), "tw", true))
	if _, ok := d.Changes[0].As[schema.DiffChangeAdd](); !ok {
		t.Errorf("change = %T, want add", d.Changes[0].Variant())
	}
	want := "diff --git /tmp/p/f.txt /tmp/p/f.txt\nnew file mode 100644\n--- /dev/null\n+++ /tmp/p/f.txt\n@@ -0,0 +1,2 @@\n+a\n+b\n"
	if d.Patch == nil || d.Patch.Text != want {
		t.Errorf("patch = %q\nwant %q", d.Patch, want)
	}
}

func TestEditResultIsModifyDiff(t *testing.T) {
	u := toolUpdate(t, updates(t, editUse, editResult), "te", true)
	d := diffOf(t, u)
	if _, ok := d.Changes[0].As[schema.DiffChangeModify](); !ok {
		t.Errorf("change = %T, want modify", d.Changes[0].Variant())
	}
	want := "diff --git /tmp/p/f.txt /tmp/p/f.txt\n--- /tmp/p/f.txt\n+++ /tmp/p/f.txt\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n"
	if d.Patch == nil || d.Patch.Text != want {
		t.Errorf("patch = %q\nwant %q", d.Patch, want)
	}
	if locs, _ := u.Locations.Get(); len(locs) != 1 || *locs[0].Line != 1 {
		t.Errorf("locations = %+v", locs)
	}
}

func TestToolErrorIsFencedText(t *testing.T) {
	u := toolUpdate(t, updates(t, readUse, readFail), "tb", true)
	if st, _ := u.Status.Get(); st != schema.ToolCallStatusFailed {
		t.Errorf("status = %v", st)
	}
	c, _ := u.Content.Get()
	cc, _ := c[0].As[schema.ToolCallContentContent]()
	if txt, _ := acp2.TextOf(cc.Content); txt != "```\nboom\n```" {
		t.Errorf("content = %q", txt)
	}
}

func TestQuoteGitPath(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b.txt":   "/a/b.txt",
		"/a/é.txt":   `"/a/\303\251.txt"`,
		"/a/b\"c":    `"/a/b\"c"`,
		"/a/b\tc":    `"/a/b\tc"`,
		"/a/b c.txt": "/a/b c.txt",
	} {
		if got := quoteGitPath(in); got != want {
			t.Errorf("quoteGitPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTodoWriteIsPlanNotToolCall(t *testing.T) {
	us := updates(t, `{"type":"assistant","message":{"id":"m","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"tt","name":"TodoWrite","input":{"todos":[{"content":"Write code","status":"in_progress","activeForm":"Writing code"},{"content":"Test","status":"pending","activeForm":"Testing"}]}}]},"parent_tool_use_id":null}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tt","content":"ok"}]},"parent_tool_use_id":null}`)
	if len(us) != 1 {
		t.Fatalf("%d updates, want the plan alone: %v", len(us), us)
	}
	p, ok := us[0].(schema.SessionUpdatePlanUpdate)
	if !ok {
		t.Fatalf("update is %T", us[0])
	}
	items, _ := p.Plan.As[schema.PlanUpdateContentItems]()
	if len(items.Entries) != 2 || items.Entries[0].Content != "Writing code" || items.Entries[0].Status != schema.PlanEntryStatusInProgress {
		t.Errorf("entries = %+v", items.Entries)
	}
}

func TestTaskToolsBuildOnePlan(t *testing.T) {
	use := func(id, name, input string) string {
		return `{"type":"assistant","message":{"id":"m` + id + `","model":"claude-sonnet-5-5","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]},"parent_tool_use_id":null}`
	}
	res := func(id, text string) string {
		return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` + text + `"}]},"parent_tool_use_id":null}`
	}
	us := updates(t,
		use("c1", "TaskCreate", `{"subject":"First","description":"d","activeForm":"Doing first"}`), res("c1", "Task #1 created successfully: First"),
		use("c2", "TaskCreate", `{"subject":"Second","description":"d"}`), res("c2", "Task #2 created successfully: Second"),
		use("u1", "TaskUpdate", `{"taskId":"1","status":"in_progress"}`), res("u1", "Updated task #1"),
		use("u2", "TaskUpdate", `{"taskId":"2","status":"deleted"}`), res("u2", "Updated task #2"),
	)
	var plans []schema.SessionUpdatePlanUpdate
	for _, u := range us {
		if p, ok := u.(schema.SessionUpdatePlanUpdate); ok {
			plans = append(plans, p)
		} else {
			t.Errorf("unexpected update %T", u)
		}
	}
	if len(plans) != 4 {
		t.Fatalf("%d plans, want one per change", len(plans))
	}
	last, _ := plans[3].Plan.As[schema.PlanUpdateContentItems]()
	if len(last.Entries) != 1 || last.Entries[0].Content != "Doing first" || last.Entries[0].Status != schema.PlanEntryStatusInProgress {
		t.Errorf("final plan = %+v", last.Entries)
	}
}

func TestUsageAndCost(t *testing.T) {
	us := updates(t, writeUse, resultOK)
	var got []schema.SessionUpdateUsageUpdate
	for _, u := range us {
		if v, ok := u.(schema.SessionUpdateUsageUpdate); ok {
			got = append(got, v)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d usage updates: %+v", len(got), got)
	}
	if got[0].Used != 2+16295+8 || got[0].Size != 1_000_000 {
		t.Errorf("used/size = %d/%d", got[0].Used, got[0].Size)
	}
	if c, ok := got[0].Cost.Get(); !ok || c.Amount != 0.05 || c.Currency != "USD" {
		t.Errorf("cost = %+v %v", c, ok)
	}
}

// The window comes from the result of the model in use: "claude-sonnet-5" finds
// the entry "claude-sonnet-5[1m]".
func TestContextWindowFromModelUsage(t *testing.T) {
	s := NewSession("id", nil, SpawnOpts{})
	s.foreground = true
	feed(s, &proc{}, writeUse, `{"type":"result","subtype":"success","result":"ok","stop_reason":"end_turn","modelUsage":{"claude-sonnet-5-5":{"contextWindow":200000}}}`)
	if w := s.tr.usage.size(); w != 200000 {
		t.Errorf("window = %d", w)
	}
}

// What Claude Code 2.1.319 printed for a manual /compact.
func TestCompaction(t *testing.T) {
	us := updates(t,
		`{"type":"system","subtype":"status","status":"compacting","uuid":"c1"}`,
		`{"type":"system","subtype":"status","status":null,"compact_result":"success","uuid":"c2"}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"c3","compact_metadata":{"trigger":"manual","pre_tokens":16218,"post_tokens":2855}}`,
		`{"type":"user","message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\n1. The user said banana.\n\nIf you need specific details from before compaction (like exact code snippets), read the full transcript at: /x.jsonl\nContinue the conversation from where it left off."},"isSynthetic":true,"isReplay":false}`,
	)
	var steps []string
	for _, u := range us {
		switch v := u.(type) {
		case schema.SessionUpdateCompactionUpdate:
			step := string(v.CompactionID) + ":" + string(v.Status)
			if sum, ok := v.Summary.Get(); ok {
				txt, _ := acp2.TextOf(sum[0])
				step += ":" + txt
			}
			steps = append(steps, step)
		case schema.SessionUpdateUsageUpdate:
			steps = append(steps, "usage")
			if v.Used != 2855 {
				t.Errorf("used = %d, want the post-compaction context", v.Used)
			}
		}
	}
	want := "c1:in_progress,c1:completed,usage,c1:completed:1. The user said banana."
	if got := strings.Join(steps, ","); got != want {
		t.Errorf("steps = %s\nwant    %s", got, want)
	}
}

func TestFailedCompaction(t *testing.T) {
	us := updates(t,
		`{"type":"system","subtype":"status","status":"compacting","uuid":"c1"}`,
		`{"type":"system","subtype":"status","status":null,"compact_result":"failed","compact_error":"too long","uuid":"c2"}`)
	last := us[len(us)-1].(schema.SessionUpdateCompactionUpdate)
	if last.Status != schema.CompactionStatusFailed {
		t.Fatalf("status = %v", last.Status)
	}
	if e, _ := last.Error.Get(); e != "too long" {
		t.Errorf("error = %q", e)
	}
}

// A compaction still open when its turn ends is cancelled, not left in progress.
func TestOpenCompactionCancelledAtTurnEnd(t *testing.T) {
	us := updates(t, `{"type":"system","subtype":"status","status":"compacting","uuid":"c1"}`, resultOK)
	var last schema.SessionUpdateCompactionUpdate
	for _, u := range us {
		if v, ok := u.(schema.SessionUpdateCompactionUpdate); ok {
			last = v
		}
	}
	if last.Status != schema.CompactionStatusCancelled {
		t.Errorf("status = %v", last.Status)
	}
}

func TestConfigOptions(t *testing.T) {
	info := &initInfo{Models: []modelInfo{
		{Value: "default", DisplayName: "Default", SupportsEffort: true, SupportedEffortLevel: []string{"low", "high"}},
		{Value: "haiku", DisplayName: "Haiku"},
	}}
	ids := func(cfg sessionConfig) []string {
		var out []string
		for _, o := range buildConfigOptions(info, cfg) {
			sel, _ := o.As[schema.SessionConfigOptionSelect]()
			out = append(out, string(sel.ConfigID)+"="+string(sel.CurrentValue))
		}
		return out
	}
	if got := strings.Join(ids(sessionConfig{}), " "); got != "mode=default model=default effort=default" {
		t.Errorf("default options = %s", got)
	}
	// A model without effort levels offers no effort option.
	if got := strings.Join(ids(sessionConfig{model: "haiku", mode: "plan"}), " "); got != "mode=plan model=haiku" {
		t.Errorf("haiku options = %s", got)
	}
	if got := strings.Join(ids(sessionConfig{effort: "high"}), " "); !strings.HasSuffix(got, "effort=high") {
		t.Errorf("effort options = %s", got)
	}
}

// A summary the API streams in its compaction block goes out as chunks, into the
// compaction Claude Code announced, and is not sent again in full when it is the
// same text.
func TestCompactionSummaryChunks(t *testing.T) {
	us := updates(t,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"compaction","content":null}}}`, // before the announcement: dropped
		`{"type":"system","subtype":"status","status":"compacting","uuid":"c1"}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"compaction","content":null}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"compaction_delta","content":"1. The user "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"compaction_delta","content":"said banana."}}}`,
		`{"type":"system","subtype":"status","status":null,"compact_result":"success","uuid":"c2"}`,
		`{"type":"user","message":{"role":"user","content":"This session is being continued from a previous conversation that ran out of context.\n\nSummary:\n1. The user said banana.\n\nIf you need specific details from before compaction (like exact code snippets), read the full transcript at: /x.jsonl\nContinue the conversation from where it left off."},"isSynthetic":true}`,
	)
	var steps []string
	for _, u := range us {
		switch v := u.(type) {
		case schema.SessionUpdateCompactionUpdate:
			steps = append(steps, string(v.Status))
			if _, ok := v.Summary.Get(); ok {
				steps[len(steps)-1] += "+summary"
			}
		case schema.SessionUpdateCompactionSummaryChunk:
			txt, _ := acp2.TextOf(v.Content)
			steps = append(steps, "chunk:"+txt)
		}
	}
	want := "in_progress,chunk:1. The user ,chunk:said banana.,completed"
	if got := strings.Join(steps, ","); got != want {
		t.Errorf("steps = %s\nwant    %s", got, want)
	}
}

func TestAvailableModes(t *testing.T) {
	info := &initInfo{Models: []modelInfo{
		{Value: "default", SupportsAutoMode: true},
		{Value: "old", SupportsAutoMode: false},
	}}
	ids := func(cfg sessionConfig) string {
		var s []string
		for _, m := range availableModes(info, cfg) {
			s = append(s, m.id)
		}
		return strings.Join(s, ",")
	}
	if got := ids(sessionConfig{}); !strings.Contains(got, "auto") || !strings.HasPrefix(got, "default,acceptEdits,plan") {
		t.Errorf("modes = %s", got)
	}
	if got := ids(sessionConfig{model: "old"}); strings.Contains(got, "auto") {
		t.Errorf("a model without Auto still offers it: %s", got)
	}
}
