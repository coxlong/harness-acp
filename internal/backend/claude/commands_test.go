package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func TestBuildCommandsFiltersNormalizesAndDeduplicates(t *testing.T) {
	info := &initInfo{
		Commands: []initCommand{
			{Name: "/compact", Description: "Compact conversation", ArgumentHint: json.RawMessage(`"[instructions]"`)},
			{Name: "clear", Description: "Clear terminal"},
			{Name: "login", Description: "Login"},
			{Name: "logout", Description: "Logout"},
			{Name: "todos", Description: "Todos"},
			{Name: "term-only", Description: "Terminal UI"},
			{Name: "github:pr (MCP)", Description: "PR prompt", ArgumentHint: json.RawMessage(`["<owner>", "<repo>"]`)},
			{Name: "bad name with space", Description: "Invalid"},
			{Name: "compact", Description: "Duplicate compact"},
			{Name: "review", Description: "Review changes"},
		},
	}
	got := buildCommands(info, []string{"term-only"})
	var names []string
	var hints []string
	for _, c := range got {
		cmd, ok := c.As[schema.AvailableCommandCommand]()
		if !ok {
			t.Fatalf("unexpected variant %T", c.Variant())
		}
		names = append(names, cmd.Name)
		hint := ""
		if txt, ok := cmd.Input.As[schema.AvailableCommandInputText](); ok {
			hint = txt.Hint
		}
		hints = append(hints, hint)
	}
	wantNames := []string{"compact", "mcp:github:pr", "review"}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("names = %v, want %v", names, wantNames)
	}
	wantHints := []string{"[instructions]", "<owner> <repo>", ""}
	if !slices.Equal(hints, wantHints) {
		t.Fatalf("hints = %v, want %v", hints, wantHints)
	}
}

func TestLiveCommandUpdatesAndLocalCommandOutput(t *testing.T) {
	s := NewSession("sess-1", nil, SpawnOpts{Cwd: "/tmp"})
	s.info = &initInfo{Commands: []initCommand{
		{Name: "compact", Description: "Compact"},
		{Name: "vim", Description: "Vim mode"},
	}}
	// system/init latches terminal_slash_commands and republishes filtered list.
	s.handle(nil, []byte(`{"type":"system","subtype":"init","terminal_slash_commands":["vim"]}`))
	ev := <-s.Events()
	upd, ok := ev.Update.Variant().(schema.SessionUpdateAvailableCommandsUpdate)
	if !ok || len(upd.AvailableCommands) != 1 {
		t.Fatalf("got %#v, want 1 filtered command", ev.Update.Variant())
	}
	if cmd, _ := upd.AvailableCommands[0].As[schema.AvailableCommandCommand](); cmd.Name != "compact" {
		t.Fatalf("remaining command = %q, want compact", cmd.Name)
	}

	// system/commands_changed replaces commands while keeping terminal filter.
	s.handle(nil, []byte(`{"type":"system","subtype":"commands_changed","commands":[{"name":"compact","description":"Compact"},{"name":"vim","description":"Vim"},{"name":"plan","description":"Plan"}]}`))
	ev = <-s.Events()
	upd, ok = ev.Update.Variant().(schema.SessionUpdateAvailableCommandsUpdate)
	if !ok || len(upd.AvailableCommands) != 2 {
		t.Fatalf("got %#v, want 2 commands after commands_changed", ev.Update.Variant())
	}

	// system/local_command_output emits agent message, except duplicate compact error.
	s.handle(nil, []byte(`{"type":"system","subtype":"status","status":"compacting","uuid":"c1"}`))
	<-s.Events()
	s.handle(nil, []byte(`{"type":"system","subtype":"status","status":null,"compact_result":"failed","compact_error":"context too small","uuid":"c2"}`))
	<-s.Events()
	// Duplicate error is suppressed.
	s.handle(nil, []byte(`{"type":"system","subtype":"local_command_output","content":"context too small","uuid":"m1"}`))
	select {
	case extra := <-s.Events():
		t.Fatalf("expected duplicate error to be suppressed, got %#v", extra)
	default:
	}
	// Normal local_command_output passes through.
	s.handle(nil, []byte(`{"type":"system","subtype":"local_command_output","content":"Context: 12%","uuid":"m2"}`))
	ev = <-s.Events()
	chunk, ok := ev.Update.Variant().(schema.SessionUpdateAgentMessageChunk)
	if !ok {
		t.Fatalf("expected agent message chunk, got %#v", ev.Update.Variant())
	}
	if txt, _ := acp2.TextOf(chunk.Content); txt != "Context: 12%" {
		t.Fatalf("text = %q", txt)
	}
}

func TestCompactionSummaryTextStripsAnalysisAndSummaryTags(t *testing.T) {
	raw := "<analysis>\ninternal reasoning\n</analysis>\n<summary>\nKept key details.\n</summary>\n"
	if got := compactionSummaryText(raw); got != "Kept key details." {
		t.Fatalf("got %q, want %q", got, "Kept key details.")
	}
}

func TestBuildUserMessageRewritesMcpSlashCommand(t *testing.T) {
	raw, err := buildUserMessage("sess-1", "msg-1", []acp2.ContentBlock{
		acp2.TextBlock("/mcp:github:pr owner/repo\nsecond line"),
		acp2.TextBlock("/mcp:other:cmd untouched"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Message.Content) != 2 {
		t.Fatalf("got %d blocks, want 2", len(env.Message.Content))
	}
	wantFirst := "/github:pr (MCP) owner/repo\nsecond line"
	if env.Message.Content[0].Text != wantFirst {
		t.Fatalf("first = %q, want %q", env.Message.Content[0].Text, wantFirst)
	}
	if env.Message.Content[1].Text != "/mcp:other:cmd untouched" {
		t.Fatalf("second text block must not be rewritten, got %q", env.Message.Content[1].Text)
	}
}

func TestUnrelatedSyntheticSummaryDoesNotOverwriteCompaction(t *testing.T) {
	tr := newTranslator()
	tr.sessionID = "sess-1"
	// Complete compaction without a summary message, then receive an unrelated
	// synthetic user message containing HTML <summary> tags.
	var l1, l2, l3 line
	_ = json.Unmarshal([]byte(`{"type":"system","subtype":"status","status":"compacting","uuid":"c1"}`), &l1)
	_ = json.Unmarshal([]byte(`{"type":"system","subtype":"status","status":null,"compact_result":"success","uuid":"c2"}`), &l2)
	_ = json.Unmarshal([]byte(`{"type":"user","isSynthetic":true,"message":{"role":"user","content":"<details><summary>Not a compaction</summary>body</details>"}}`), &l3)
	tr.system(&l1)
	tr.system(&l2)
	if evs := tr.content(&l3); len(evs) != 0 {
		t.Fatalf("expected unrelated synthetic <summary> to be ignored, got %#v", evs)
	}
	// Turn-end reset must also clear lastCompaction and duplicateCompactErr.
	tr.duplicateCompactErr = "stale error"
	tr.compactionCancel()
	if tr.lastCompaction != "" || tr.duplicateCompactErr != "" {
		t.Fatalf("expected turn reset to clear compaction state: last=%q dup=%q", tr.lastCompaction, tr.duplicateCompactErr)
	}
}

func TestReplayCompactSummaryEmitsCompactionUpdate(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "sess.jsonl")
	lines := []string{
		`{"type":"user","uuid":"u1","cwd":"/work","message":{"role":"user","content":"hi"}}`,
		`{"type":"user","uuid":"cmp-1","cwd":"/work","isMeta":true,"isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation.\n\nSummary:\nDiscussed ACP commands.\n\nContinue the conversation from where it left off."}}`,
		`{"type":"user","uuid":"u2","cwd":"/work","message":{"role":"user","content":"next"}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := readTranscript(transcript, false)
	if err != nil {
		t.Fatal(err)
	}
	r := newReplayer("sess", transcript, "/work")
	r.scan(parsed, "sess")
	r.newHistory("sess", nil).run(parsed)

	var found *schema.SessionUpdateCompactionUpdate
	for _, u := range r.out {
		if cmp, ok := u.Update.Variant().(schema.SessionUpdateCompactionUpdate); ok {
			found = &cmp
		}
	}
	if found == nil {
		t.Fatalf("expected replayed compaction_update, got %d updates", len(r.out))
	}
	if found.CompactionID != "cmp-1" || found.Status != schema.CompactionStatusCompleted {
		t.Fatalf("compaction = %+v", found)
	}
	sum, ok := found.Summary.Get()
	if !ok || len(sum) != 1 {
		t.Fatalf("expected 1 summary block, got %+v", found.Summary)
	}
	if txt, _ := acp2.TextOf(sum[0]); txt != "Discussed ACP commands." {
		t.Fatalf("summary = %q", txt)
	}
}
