package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
)

// write puts a file in place, making its directory.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Deleting a session removes its transcript in every project directory, its
// subagents' transcripts, and the per-session scratch beside them, while
// leaving other sessions alone.
func TestDeleteSessionRemovesTranscriptAndScratch(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	const id = "5d0f9a1e-gone"
	project := filepath.Join(cfg, "projects", "-work-a")

	write(t, filepath.Join(project, id+".jsonl"), `{"type":"user"}`)
	write(t, filepath.Join(cfg, "projects", "-work-b", id+".jsonl"), `{"type":"user"}`)
	write(t, filepath.Join(project, id, "subagent.jsonl"), `{}`)
	write(t, filepath.Join(cfg, "file-history", id, "snapshot"), "x")
	write(t, filepath.Join(cfg, "tasks", id, "t1"), "x")
	write(t, filepath.Join(cfg, "session-env", id, "env"), "x")
	kept := filepath.Join(project, "5d0f9a1e-kept.jsonl")
	write(t, kept, `{"type":"user"}`)

	if err := (&Backend{}).DeleteSession(context.Background(), id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	for _, gone := range []string{
		filepath.Join(project, id+".jsonl"),
		filepath.Join(cfg, "projects", "-work-b", id+".jsonl"),
		filepath.Join(project, id),
		filepath.Join(cfg, "file-history", id),
		filepath.Join(cfg, "tasks", id),
		filepath.Join(cfg, "session-env", id),
	} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there (err = %v)", gone, err)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("another session's transcript was removed: %v", err)
	}
}

// The protocol asks that deleting an unknown session succeed: what the caller
// wants is that it is gone, and it is.
func TestDeleteSessionUnknownIsFine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := (&Backend{}).DeleteSession(context.Background(), "never-existed"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
}

// An id that is not a plain file name is a parameter problem, not a session.
func TestDeleteSessionInvalidID(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, id := range []string{"", "../escape", "a/b", `a\b`, "with.dot"} {
		err := (&Backend{}).DeleteSession(context.Background(), id)
		var param *backend.InvalidParamsError
		if !errors.As(err, &param) {
			t.Errorf("%q: err = %v, want an InvalidParamsError", id, err)
		}
	}
}
