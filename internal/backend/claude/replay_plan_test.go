package claude

import (
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
	if _, ok := last.Update.Variant().(schema.SessionUpdatePlanRemoved); !ok {
		t.Fatalf("last update = %T, want plan_removed", last.Update.Variant())
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
		case schema.SessionUpdatePlanUpdate, schema.SessionUpdatePlanRemoved:
			t.Fatalf("unexpected plan update: %T", u.Update.Variant())
		}
	}
}
