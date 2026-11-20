package claude

import (
	"encoding/json"
	"strings"
	"testing"

	schema "github.com/ironpark/acp-go/schema/v2"
)

// A resumed session must show the plan it ended with: the protocol lists plans
// among the history a replay reports.
func TestReplayPlanFromTasks(t *testing.T) {
	root := []string{
		usr("u1", `"build it"`, false, ""),
		asst("m1", "a1", use("t1", "TaskCreate", `{"subject":"Write code","activeForm":"Writing code"}`), false),
		usr("u2", result("t1", `"Task #1 created successfully: Write code"`, false), false, ""),
		asst("m2", "a2", use("t2", "TaskCreate", `{"subject":"Run tests"}`), false),
		usr("u3", result("t2", `"Task #2 created successfully: Run tests"`, false), false, ""),
		asst("m3", "a3", use("t3", "TaskUpdate", `{"taskId":"1","status":"completed"}`), false),
		usr("u4", result("t3", `"Updated task #1 status"`, false), false, ""),
	}
	transcriptDir(t, "s-plan", root, nil, nil)

	lines, h := historyLines(t, "s-plan")
	// The task tools are the plan, not tool cards.
	for _, l := range lines {
		if l == "tool:t1 in_progress" || l == "tool:t2 in_progress" || l == "tool:t3 in_progress" {
			t.Errorf("%s: task tool replayed as a tool card", l)
		}
	}

	last := h.Updates[len(h.Updates)-1]
	p, ok := last.Update.Variant().(schema.SessionUpdatePlanUpdate)
	if !ok {
		t.Fatalf("last update = %T, want plan_update", last.Update.Variant())
	}
	items, ok := p.Plan.As[schema.PlanUpdateContentItems]()
	if !ok {
		t.Fatalf("plan = %v, want items", p.Plan)
	}
	if len(items.Entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(items.Entries), items.Entries)
	}
	if items.Entries[0].Content != "Write code" || items.Entries[0].Status != schema.PlanEntryStatusCompleted {
		t.Errorf("entry 0 = %+v, want completed Write code", items.Entries[0])
	}
	if items.Entries[1].Content != "Run tests" || items.Entries[1].Status != schema.PlanEntryStatusPending {
		t.Errorf("entry 1 = %+v, want pending Run tests", items.Entries[1])
	}
}

// TodoWrite carried the whole list itself, and old transcripts still hold it.
func TestReplayPlanFromTodoWrite(t *testing.T) {
	root := []string{
		usr("u1", `"go"`, false, ""),
		asst("m1", "a1", use("t1", "TodoWrite", `{"todos":[{"content":"Step one","status":"in_progress","activeForm":"Doing step one"},{"content":"Step two","status":"pending"}]}`), false),
		usr("u2", result("t1", `"Todos have been modified"`, false), false, ""),
	}
	transcriptDir(t, "s-todo", root, nil, nil)

	_, h := historyLines(t, "s-todo")
	last := h.Updates[len(h.Updates)-1]
	p, ok := last.Update.Variant().(schema.SessionUpdatePlanUpdate)
	if !ok {
		t.Fatalf("last update = %T, want plan_update", last.Update.Variant())
	}
	items, _ := p.Plan.As[schema.PlanUpdateContentItems]()
	if len(items.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(items.Entries))
	}
	if items.Entries[0].Content != "Doing step one" {
		t.Errorf("in-progress entry = %q, want the active form", items.Entries[0].Content)
	}
}

// A plan that ended empty is removed, so the client drops the band instead of
// showing a stale one.
func TestReplayPlanCleared(t *testing.T) {
	root := []string{
		asst("m1", "a1", use("t1", "TaskCreate", `{"subject":"Only step"}`), false),
		usr("u2", result("t1", `"Task #1 created successfully: Only step"`, false), false, ""),
		asst("m2", "a2", use("t2", "TaskUpdate", `{"taskId":"1","status":"deleted"}`), false),
		usr("u3", result("t2", `"Deleted task #1"`, false), false, ""),
	}
	transcriptDir(t, "s-clear", root, nil, nil)

	_, h := historyLines(t, "s-clear")
	last := h.Updates[len(h.Updates)-1]
	p, ok := last.Update.Variant().(schema.SessionUpdatePlanUpdate)
	if !ok {
		t.Fatalf("last update = %T, want an empty plan_update", last.Update.Variant())
	}
	items, _ := p.Plan.As[schema.PlanUpdateContentItems]()
	if len(items.Entries) != 0 {
		t.Fatalf("entries = %d, want none", len(items.Entries))
	}
	// An empty array, not null: the wire form has to be the one the schema
	// names for clearing a plan.
	raw, err := json.Marshal(last.Update)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"entries":[]`) {
		t.Fatalf("update = %s, want an empty entries array", raw)
	}
}

// A transcript with no plan at all must not announce one.
func TestReplayWithoutPlanSaysNothing(t *testing.T) {
	root := []string{
		usr("u1", `"hi"`, false, ""),
		asst("m1", "a1", `{"type":"text","text":"hello"}`, false),
	}
	transcriptDir(t, "s-none", root, nil, nil)

	_, h := historyLines(t, "s-none")
	for _, u := range h.Updates {
		switch u.Update.Variant().(type) {
		case schema.SessionUpdatePlanUpdate:
			t.Fatalf("unexpected plan update: %T", u.Update.Variant())
		}
	}
}

// A plan belongs to one session: a subagent that keeps its own checklist must
// not turn up in the parent's, and each session replays its own.
func TestReplayPlanStaysWithItsSession(t *testing.T) {
	sub := []string{
		usr("su1", `"help"`, true, ""),
		asst("sm1", "sa1", use("st1", "TaskCreate", `{"subject":"Sub work"}`), true),
		usr("su2", result("st1", `"Task #1 created successfully: Sub work"`, false), true, ""),
	}
	root := []string{
		usr("u1", `"go"`, false, ""),
		asst("m1", "a1", use("p1", "TaskCreate", `{"subject":"Parent work"}`), false),
		usr("u2", result("p1", `"Task #1 created successfully: Parent work"`, false), false, ""),
		asst("m2", "a2", use("tu1", "Agent", `{"description":"Help","prompt":"help"}`), false),
		usr("u3", result("tu1", `"done"`, false), false, ""),
	}
	transcriptDir(t, "s-scope", root, map[string][]string{"a1sub": sub},
		map[string]string{"a1sub": `{"agentType":"general-purpose","description":"Help","toolUseId":"tu1","requestShape":"foreground"}`})

	_, h := historyLines(t, "s-scope")
	plans := 0
	for _, u := range h.Updates {
		p, ok := u.Update.Variant().(schema.SessionUpdatePlanUpdate)
		if !ok {
			continue
		}
		plans++
		items, _ := p.Plan.As[schema.PlanUpdateContentItems]()
		if len(items.Entries) != 1 {
			t.Fatalf("entries = %d, want 1", len(items.Entries))
		}
		want := "Parent work"
		if u.Session != "" {
			want = "Sub work"
		}
		if items.Entries[0].Content != want {
			t.Errorf("session %q plan = %q, want %q", u.Session, items.Entries[0].Content, want)
		}
	}
	if plans != 2 {
		t.Fatalf("plan updates = %d, want one per session (2)", plans)
	}
}
