package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
)

// Backend is the process-level singleton for the claude agent.
type Backend struct {
	rt *Runtime

	optMu    sync.Mutex
	optCache map[string]cachedOptions // by cwd
}

type cachedOptions struct {
	at   time.Time
	opts backend.Options
}

// optionsTTL is how long session/options answers from memory: starting a
// process to ask costs about a second.
const optionsTTL = time.Minute

var _ backend.Backend = (*Backend)(nil)

// NewBackend creates the claude backend over rt.
func NewBackend(rt *Runtime) *Backend {
	return &Backend{rt: rt, optCache: map[string]cachedOptions{}}
}

func (b *Backend) Name() string { return "claude" }

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// Probe reports the installed claude version.
func (b *Backend) Probe(ctx context.Context) (backend.AgentInfo, error) {
	info := backend.AgentInfo{Name: "claude", DisplayName: "Claude"}
	out, err := exec.CommandContext(ctx, b.rt.cfg.Binary, "--version").Output()
	if err != nil {
		return info, fmt.Errorf("claude --version: %w", err)
	}
	info.Version = versionRe.FindString(string(out))
	info.Available = info.Version != ""
	return info, nil
}

// Options reports what a session started in cwd offers. It asks a process
// that lives only for the question, and remembers the answer for a minute.
func (b *Backend) Options(ctx context.Context, cwd string, dirs []string) (backend.Options, error) {
	b.optMu.Lock()
	c, ok := b.optCache[cwd]
	b.optMu.Unlock()
	if ok && time.Since(c.at) < optionsTTL {
		return c.opts, nil
	}
	s := NewSession(newUUID(), b.rt, SpawnOpts{Cwd: cwd, AddDirs: dirs, New: true})
	defer s.Close(ctx)
	if err := s.Init(ctx); err != nil {
		return backend.Options{}, err
	}
	opts := s.Options()
	b.optMu.Lock()
	b.optCache[cwd] = cachedOptions{at: time.Now(), opts: opts}
	b.optMu.Unlock()
	return opts, nil
}

// NewSession creates a session and starts its process, so that the session can
// report its options at once. The process keeps --session-id until the first
// prompt creates the transcript.
func (b *Backend) NewSession(ctx context.Context, p backend.NewSessionParams) (backend.SessionHandle, error) {
	s := NewSession(newUUID(), b.rt, SpawnOpts{
		Cwd:      p.Cwd,
		AddDirs:  p.AdditionalDirectories,
		New:      true,
		Model:    p.Model,
		SystemPr: p.Instructions,
	})
	if err := s.Init(ctx); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	return s, nil
}

// ResumeSession attaches to an existing session and starts its process, with
// --resume. Claude resolves --resume against the project directory of its cwd,
// so the cwd recorded in the transcript wins over the request's: a client may
// reopen a session from another directory.
func (b *Backend) ResumeSession(ctx context.Context, id string, p backend.ResumeParams) (backend.SessionHandle, error) {
	path, err := findTranscript(id)
	if err != nil {
		return nil, err
	}
	cwd := p.Cwd
	titled := false
	if meta, err := scanTranscript(path); err == nil {
		titled = meta.title() != ""
		if meta.cwd != "" {
			cwd = meta.cwd
		}
	}
	s := NewSession(id, b.rt, SpawnOpts{Cwd: cwd, AddDirs: p.AdditionalDirectories})
	s.titled = titled // a title the session has is not made again
	if err := s.Init(ctx); err != nil {
		_ = s.Close(ctx)
		return nil, err
	}
	return s, nil
}

func (b *Backend) Close() error {
	b.rt.Close()
	return nil
}

// --- transcripts ------------------------------------------------------------

func configDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// findTranscript locates a session's transcript in any project directory.
func findTranscript(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return "", fmt.Errorf("invalid session id %q", id)
	}
	matches, _ := filepath.Glob(filepath.Join(configDir(), "projects", "*", id+".jsonl"))
	if len(matches) == 0 {
		return "", fmt.Errorf("session %s not found", id)
	}
	return matches[0], nil
}

// userText returns the text of a user message: its string content, or its
// text blocks joined. Tool results are not text.
func userText(raw json.RawMessage) string {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// hiddenUserText reports text Claude Code wrote into the transcript that the
// live session never showed as a prompt: the interrupt marker, and command and
// reminder wrappers.
func hiddenUserText(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "[Request interrupted by user") ||
		strings.HasPrefix(t, "<task-notification>") ||
		strings.HasPrefix(t, "<local-command") ||
		strings.HasPrefix(t, "<command-name>") ||
		strings.HasPrefix(t, "<system-reminder>")
}
