package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// transcriptDir writes a session transcript and its subagents' as Claude Code
// lays them out, and points the backend at it.
func transcriptDir(t *testing.T, id string, root []string, subs map[string][]string, metas map[string]string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := filepath.Join(cfg, "projects", "-work")
	if err := os.MkdirAll(filepath.Join(proj, id, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path string, lines []string) {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(proj, id+".jsonl"), root)
	for agent, lines := range subs {
		write(filepath.Join(proj, id, "subagents", "agent-"+agent+".jsonl"), lines)
	}
	for agent, meta := range metas {
		write(filepath.Join(proj, id, "subagents", "agent-"+agent+".meta.json"), []string{meta})
	}
}

func asst(id, uuid, block string, sidechain bool) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"%s","isSidechain":%v,"cwd":"/work","message":{"id":"%s","model":"m","content":[%s]}}`, uuid, sidechain, id, block)
}

func usr(uuid, content string, sidechain bool, extra string) string {
	return fmt.Sprintf(`{"type":"user","uuid":"%s","isSidechain":%v,"cwd":"/work",%s"message":{"role":"user","content":%s}}`, uuid, sidechain, extra, content)
}

func use(id, name, input string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":"%s","name":"%s","input":%s}`, id, name, input)
}

func result(id, content string, isErr bool) string {
	return fmt.Sprintf(`[{"type":"tool_result","tool_use_id":"%s","content":%s,"is_error":%v}]`, id, content, isErr)
}

func historyLines(t *testing.T, id string) ([]string, backend.History) {
	t.Helper()
	h, err := (&Backend{}).History(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, u := range h.Updates {
		pre := ""
		if u.Session != "" {
			pre = "[" + u.Session + "] "
		}
		switch v := u.Update.Variant().(type) {
		case schema.SessionUpdateUserMessage:
			c, _ := v.Content.Get()
			txt, _ := acp2.TextOf(c[0])
			out = append(out, fmt.Sprintf("%suser:%q", pre, txt))
		case schema.SessionUpdateAgentMessage:
			c, _ := v.Content.Get()
			txt, _ := acp2.TextOf(c[0])
			out = append(out, fmt.Sprintf("%sagent:%q", pre, txt))
		case schema.SessionUpdateToolCallUpdate:
			st, _ := v.Status.Get()
			d := fmt.Sprintf("%stool:%s %s", pre, v.ToolCallID, st)
			if c, ok := v.Content.Get(); ok {
				for _, x := range c {
					if s, ok := x.As[schema.ToolCallContentSession](); ok {
						d += " ->" + string(s.SessionID)
					}
				}
			}
			out = append(out, d)
		case schema.SessionUpdateSubagentUpdate:
			d := "subagent:" + string(v.SessionID)
			if ti, ok := v.Title.Get(); ok {
				d += " title=" + ti
			}
			if de, ok := v.Description.Get(); ok {
				d += " desc=" + de
			}
			out = append(out, pre+d)
		case schema.SessionUpdateSessionMessage:
			c, _ := v.Content.Get()
			txt, _ := acp2.TextOf(c[0])
			from, _ := v.SenderSessionID.Get()
			to, _ := v.RecipientSessionID.Get()
			out = append(out, fmt.Sprintf("%smessage:%s %s->%s %q", pre, v.MessageID, from, to, txt))
		}
	}
	return out, h
}

const (
	fgMeta  = `{"agentType":"general-purpose","description":"Answer 2+2","toolUseId":"tu1","requestShape":"foreground"}`
	bgMeta  = `{"agentType":"general-purpose","description":"Watch","toolUseId":"tu2","requestShape":"background","stoppedByUser":true}`
	subText = `{"type":"text","text":"4"}`
)

func TestReplaySubagents(t *testing.T) {
	root := []string{
		usr("u1", `"go"`, false, ""),
		asst("m1", "a1", use("tu1", "Agent", `{"description":"Answer 2+2","subagent_type":"general-purpose","prompt":"What is 2+2?"}`), false),
		usr("u2", result("tu1", `"4\nagentId: a1x (use SendMessage)"`, false), false, ""),
		asst("m2", "a2", use("tu2", "Agent", `{"description":"Watch","subagent_type":"general-purpose","prompt":"Watch it","run_in_background":true}`), false),
		usr("u3", result("tu2", `"launched"`, false), false, ""),
		asst("m3", "a3", use("tu9", "Agent", `{"description":"Lost","prompt":"x"}`), false),
		usr("u4", result("tu9", `"lost"`, false), false, ""),
		asst("m4", "a4", use("tu4", "TaskStop", `{"task_id":"b2x"}`), false),
		usr("u5", result("tu4", `"stopped"`, false), false, ""),
		asst("m5", "a5", `{"type":"text","text":"done"}`, false),
	}
	subs := map[string][]string{
		"a1x": {
			usr("s1", `"What is 2+2?"`, true, ""),
			asst("sm1", "sa1", subText, true),
		},
		"b2x": {
			usr("s2", `"Watch it"`, true, ""),
			asst("sm2", "sa2", use("tw", "Bash", `{"command":"sleep 1"}`), true),
		},
	}
	transcriptDir(t, "sess", root, subs, map[string]string{"a1x": fgMeta, "b2x": bgMeta})
	got, h := historyLines(t, "sess")
	want := `user:"go"
subagent:a1x title=Answer 2+2 desc=general-purpose
message:tu1 sess->a1x "What is 2+2?"
[a1x] message:tu1 sess->a1x "What is 2+2?"
[a1x] agent:"4"
message:tu1:reply a1x->sess "4"
subagent:b2x title=Watch desc=general-purpose
message:tu2 sess->b2x "Watch it"
[b2x] message:tu2 sess->b2x "Watch it"
[b2x] tool:tw in_progress
[b2x] tool:tw failed
tool:tu9 in_progress
tool:tu9 completed
tool:tu4 in_progress ->b2x
tool:tu4 completed ->b2x
agent:"done"`
	if g := strings.Join(got, "\n"); g != want {
		t.Errorf("replay:\n%s\nwant:\n%s", g, want)
	}
	if h.Missing != 1 {
		t.Errorf("missing = %d, want the launch with no meta", h.Missing)
	}
	if len(h.Children) != 2 {
		t.Fatalf("children = %+v", h.Children)
	}
	// A foreground launch that returned ended; the user stopped the other.
	if c := h.Children[0]; c.ID != "a1x" || c.Parent != "" || c.End.StopReason != acp2.StopReasonEndTurn {
		t.Errorf("first child = %+v", c)
	}
	if c := h.Children[1]; c.End.StopReason != acp2.StopReasonCancelled {
		t.Errorf("second child = %+v", c)
	}
}

// A child that handed back its report, a message delivered by SendMessage, and
// a task notification that says how a child ended.
func TestReplayHandbackDeliveryAndNotification(t *testing.T) {
	root := []string{
		asst("m1", "a1", use("tu1", "Agent", `{"description":"Work","prompt":"start","name":"worker"}`), false),
		usr("u2", result("tu1", `"handed back"`, false), false, ""),
		asst("m2", "a2", use("tu5", "SendMessage", `{"to":"w1x","message":"keep going"}`), false),
		usr("u3", result("tu5", `"sent"`, false), false, ""),
		usr("u4", `"<task-notification>\n<task-id>w1x</task-id>\n<status>failed</status>\n<result>boom</result>\n</task-notification>"`, false, ""),
	}
	subs := map[string][]string{"w1x": {
		usr("s1", `"start"`, true, ""),
		asst("sm1", "sa1", use("th", handbackTool, `{"message":"my report"}`), true),
		usr("s2", result("th", `"ok"`, false), true, ""),
		usr("s3", `"The coordinator sent a message while you were working:\nkeep going\nRespond."`, true, `"origin":{"kind":"coordinator"},`),
	}}
	transcriptDir(t, "sess2", root, subs, map[string]string{
		"w1x": `{"agentType":"general-purpose","name":"worker","description":"Work","toolUseId":"tu1","requestShape":"foreground"}`,
	})
	got, h := historyLines(t, "sess2")
	want := `subagent:w1x title=worker desc=general-purpose
message:tu1 sess2->w1x "start"
[w1x] message:tu1 sess2->w1x "start"
[w1x] message:th w1x->sess2 "my report"
message:th w1x->sess2 "my report"
[w1x] message:tu5 sess2->w1x "keep going"
message:tu5 sess2->w1x "keep going"`
	if g := strings.Join(got, "\n"); g != want {
		t.Errorf("replay:\n%s\nwant:\n%s", g, want)
	}
	if c := h.Children[0]; c.End.Err == nil {
		t.Errorf("the notification's failure was lost: %+v", c.End)
	}
}

// A launch with no meta file stays a tool call; a session with no subagents
// replays as before.
func TestReplayWithoutSubagents(t *testing.T) {
	root := []string{usr("u1", `"hi"`, false, ""), asst("m1", "a1", `{"type":"text","text":"hello"}`, false)}
	transcriptDir(t, "sess3", root, nil, nil)
	got, h := historyLines(t, "sess3")
	if strings.Join(got, "|") != `user:"hi"|agent:"hello"` || len(h.Children) != 0 || h.Missing != 0 {
		t.Errorf("replay = %v %+v", got, h)
	}
}
