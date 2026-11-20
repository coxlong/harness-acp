package claude

import (
	"context"
	"os"
	"path/filepath"
)

// DeleteSession removes everything the CLI keeps for one session: its
// transcript, its subagents' transcripts, and the per-session scratch beside
// them. A session that is not there is not an error, as the protocol asks:
// what the caller wants is that it is gone.
//
// The store is the CLI's own directory tree, so the paths are guessed from the
// same layout listing and resume use. Anything that is not there is skipped;
// a removal that fails for another reason (permissions, mostly) is reported.
func (b *Backend) DeleteSession(ctx context.Context, id string) error {
	if !validSessionID(id) {
		return invalidSessionID(id)
	}
	base := configDir()
	targets := make([]string, 0, 8)
	add := func(pattern string) {
		matches, _ := filepath.Glob(pattern)
		targets = append(targets, matches...)
	}
	// The transcript is the session; the directory beside it holds any
	// subagent transcripts it launched.
	add(filepath.Join(base, "projects", "*", id+".jsonl"))
	add(filepath.Join(base, "projects", "*", id))
	// Scratch the CLI keeps per session, next to its transcripts.
	for _, dir := range []string{"file-history", "tasks", "session-env", "sessions"} {
		add(filepath.Join(base, dir, id))
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
