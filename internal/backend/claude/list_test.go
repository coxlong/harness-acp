package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
)

// writeSession writes a transcript into a fake config dir and sets its mtime.
func writeSession(t *testing.T, cfg, project, id string, age time.Duration, lines ...string) {
	t.Helper()
	dir := filepath.Join(cfg, "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Add(-age)
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func user(cwd, text string) string {
	return fmt.Sprintf(`{"type":"user","cwd":%q,"message":{"role":"user","content":%q}}`, cwd, text)
}

func list(t *testing.T, f backend.ListFilter) ([]backend.SessionInfo, bool) {
	t.Helper()
	out, more, err := (&Backend{}).ListSessions(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return out, more
}

func TestListTitleOrder(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	d := "/work/a"
	// first prompt only
	writeSession(t, cfg, "-work-a", "s-first", 5*time.Hour, user(d, "build the thing\nplease"))
	// the last prompt wins over the first
	writeSession(t, cfg, "-work-a", "s-last", 4*time.Hour, user(d, "first"), `{"type":"last-prompt","lastPrompt":"latest"}`)
	// a generated title wins over the last prompt; the user's /rename over both
	writeSession(t, cfg, "-work-a", "s-ai", 3*time.Hour, user(d, "first"), `{"type":"last-prompt","lastPrompt":"latest"}`, `{"type":"ai-title","aiTitle":"Generated"}`)
	writeSession(t, cfg, "-work-a", "s-custom", 2*time.Hour, user(d, "first"), `{"type":"ai-title","aiTitle":"Generated"}`, `{"type":"custom-title","customTitle":"My name"}`, `{"type":"custom-title","customTitle":"Renamed twice"}`)

	got, _ := list(t, backend.ListFilter{})
	want := map[string]string{"s-first": "build the thing please", "s-last": "latest", "s-ai": "Generated", "s-custom": "Renamed twice"}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(got), len(want))
	}
	for _, s := range got {
		if s.Title != want[s.ID] {
			t.Errorf("%s: title %q, want %q", s.ID, s.Title, want[s.ID])
		}
		if s.UpdatedAt.IsZero() {
			t.Errorf("%s: no UpdatedAt", s.ID)
		}
	}
	// newest first
	if got[0].ID != "s-custom" || got[3].ID != "s-first" {
		t.Errorf("order: %s ... %s", got[0].ID, got[3].ID)
	}
}

// A session with no prompt of its own shows no title and is left out; so is one
// with no directory. Commands, tags, interrupt markers and tool results are no prompt.
func TestListFirstPromptRules(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	d := "/work/a"
	writeSession(t, cfg, "-work-a", "s-skip", time.Hour,
		user(d, "<command-name>/clear</command-name>"),
		user(d, "[Request interrupted by user]"),
		user(d, "<system-reminder>x</system-reminder>"),
		user(d, "real prompt"))
	writeSession(t, cfg, "-work-a", "s-command-only", 2*time.Hour, user(d, "<command-name>/compact</command-name>"))
	writeSession(t, cfg, "-work-a", "s-empty", 3*time.Hour, `{"type":"queue-operation"}`)
	writeSession(t, cfg, "-work-a", "s-nocwd", 4*time.Hour, `{"type":"user","message":{"role":"user","content":"hi"}}`)
	writeSession(t, cfg, "-work-a", "s-bash", 5*time.Hour, user(d, "<bash-input>ls -la</bash-input>"))
	writeSession(t, cfg, "-work-a", "s-sidechain", 6*time.Hour,
		fmt.Sprintf(`{"type":"user","isSidechain":true,"cwd":%q,"message":{"role":"user","content":"sub"}}`, d))

	got, _ := list(t, backend.ListFilter{})
	titles := map[string]string{}
	for _, s := range got {
		titles[s.ID] = s.Title
	}
	for id, want := range map[string]string{"s-skip": "real prompt", "s-command-only": "/compact", "s-bash": "! ls -la"} {
		if titles[id] != want {
			t.Errorf("%s: title %q, want %q", id, titles[id], want)
		}
	}
	for _, id := range []string{"s-empty", "s-nocwd", "s-sidechain"} {
		if _, ok := titles[id]; ok {
			t.Errorf("%s should not be listed", id)
		}
	}
}

func TestListCwdFilterAndPaging(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for i := 0; i < 5; i++ {
		writeSession(t, cfg, "-work-a", fmt.Sprintf("a%d", i), time.Duration(i)*time.Minute, user("/work/a", "p"))
	}
	writeSession(t, cfg, "-work-b", "b0", 0, user("/work/b", "p"))
	// a subdirectory is not the same directory
	writeSession(t, cfg, "-work-a-sub", "sub0", 0, user("/work/a/sub", "p"))

	got, more := list(t, backend.ListFilter{Cwd: "/work/a"})
	if len(got) != 5 || more {
		t.Fatalf("cwd filter: %d sessions more=%v, want 5 false", len(got), more)
	}

	var ids []string
	offset := 0
	for page := 0; page < 10; page++ {
		got, more := list(t, backend.ListFilter{Cwd: "/work/a", Offset: offset, Limit: 2})
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		offset += 2
		if !more {
			break
		}
	}
	if strings.Join(ids, ",") != "a0,a1,a2,a3,a4" {
		t.Errorf("paged ids = %v", ids)
	}
	// exactly a full last page does not promise another
	if _, more := list(t, backend.ListFilter{Cwd: "/work/a", Offset: 3, Limit: 2}); more {
		t.Error("the last page must not set more")
	}
}

func TestSanitizeTitle(t *testing.T) {
	if got := sanitizeTitle("a\n\n  b\tc "); got != "a b c" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 300)
	got := sanitizeTitle(long)
	if r := []rune(got); len(r) != maxTitle || r[maxTitle-1] != '…' {
		t.Errorf("long title: %d runes", len(r))
	}
}
