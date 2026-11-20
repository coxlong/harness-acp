package claude

import (
	"os"
	"strings"
	"testing"
)

// The task tools are what the client's plan panel shows, and the agent only
// loads them with this switch on: an agent process must always get it.
func TestSpawnEnvTurnsOnTaskTools(t *testing.T) {
	// t.Setenv restores the value afterwards; unsetting it here is what a stock
	// deployment looks like, with no choice made yet.
	t.Setenv("CLAUDE_CODE_ENABLE_TODO_TOOLS", "")
	os.Unsetenv("CLAUDE_CODE_ENABLE_TODO_TOOLS")
	env := spawnEnv()
	if got := valueOf(env, "CLAUDE_CODE_ENABLE_TODO_TOOLS"); got != "true" {
		t.Fatalf("CLAUDE_CODE_ENABLE_TODO_TOOLS = %q, want true", got)
	}
}

// An operator's explicit choice wins: harnessd does not override configuration.
func TestSpawnEnvKeepsOperatorChoice(t *testing.T) {
	t.Setenv("CLAUDE_CODE_ENABLE_TODO_TOOLS", "false")
	env := spawnEnv()
	if got := valueOf(env, "CLAUDE_CODE_ENABLE_TODO_TOOLS"); got != "false" {
		t.Fatalf("CLAUDE_CODE_ENABLE_TODO_TOOLS = %q, want false", got)
	}
	if n := countVar(env, "CLAUDE_CODE_ENABLE_TODO_TOOLS"); n != 1 {
		t.Fatalf("found %d entries, want 1", n)
	}
}

func valueOf(env []string, name string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, name+"="); ok {
			return v
		}
	}
	return ""
}

func countVar(env []string, name string) int {
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, name+"=") {
			n++
		}
	}
	return n
}
