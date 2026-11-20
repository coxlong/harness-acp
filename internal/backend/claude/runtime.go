// Package claude implements the backend for Claude Code CLI sessions.
//
// Each session owns one `claude --print --input-format stream-json` process.
// The protocol (verified against Claude Code 2.1.319):
//
//   - stdin: one JSON user message per line; control_request lines
//     (interrupt) and control_response lines (permission answers).
//   - stdout: system/stream_event/assistant/user messages, control_request
//     (can_use_tool) when --permission-prompt-tool=stdio is set, and one
//     `result` message that ends each turn.
package claude

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
)

// spawnEnv is the environment for an agent process.
//
// The client's plan panel is fed by the task tools (TaskCreate, TaskUpdate,
// TaskList, TaskGet). The CLI loads them only for a model it recognizes; behind
// a custom model name or an inference profile they are left out, and a session
// then has no way to report a plan at all. The CLI's own switch for this is
// turned on here unless the operator set it, so the toolset does not depend on
// how harnessd happens to be launched.
func spawnEnv() []string {
	const flag = "CLAUDE_CODE_ENABLE_TODO_TOOLS"
	env := os.Environ()
	for _, e := range env {
		if strings.HasPrefix(e, flag+"=") {
			return env
		}
	}
	return append(env, flag+"=true")
}

// RuntimeConfig controls per-runtime limits.
type RuntimeConfig struct {
	// IdleTTL is how long an idle process lives before GC kills it.
	// Zero disables GC.
	IdleTTL time.Duration
	// MaxProcs caps live processes. Zero means unlimited.
	MaxProcs int
	// Binary is the claude executable; empty means "claude" from PATH.
	Binary string
}

// SpawnOpts describes how to start a session's process.
type SpawnOpts struct {
	Cwd      string
	AddDirs  []string
	New      bool // true: --session-id (fresh); false: --resume
	Model    string
	Mode     string // permission mode; "" for Claude Code's default
	Effort   string // effort level; "" for Claude Code's default
	SystemPr string
}

// proc is one running claude subprocess.
type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser // captured before cmd.Start()
	cancel context.CancelFunc

	// stderr keeps the tail of the process's stderr for error reports.
	stderr *tail

	// done is closed once the process has been reaped (cmd.Wait returned).
	// Whoever drains stdout calls reap; nothing else calls Wait.
	done chan struct{}

	// busy is set while a turn is in flight so GC never kills mid-turn work.
	busy     atomic.Bool
	mu       sync.Mutex // guards lastUsed and stdin writes
	lastUsed time.Time
}

// alive reports whether the process has not been reaped yet.
func (p *proc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// write sends one NDJSON line to the process, serialised against other writers.
func (p *proc) write(line []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.alive() {
		return fmt.Errorf("claude process exited")
	}
	_, err := p.stdin.Write(append(line, '\n'))
	return err
}

func (p *proc) touch() {
	p.mu.Lock()
	p.lastUsed = time.Now()
	p.mu.Unlock()
}

// reap waits for the process and marks it done. Call after stdout hit EOF.
func (p *proc) reap() {
	_ = p.cmd.Wait()
	p.cancel()
	close(p.done)
}

// Runtime manages per-session claude subprocesses, keyed by session UUID.
type Runtime struct {
	cfg   RuntimeConfig
	ctx   context.Context
	mu    sync.Mutex
	procs map[string]*proc
}

// NewRuntime creates a Runtime and starts its GC goroutine.
func NewRuntime(ctx context.Context, cfg RuntimeConfig) *Runtime {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	r := &Runtime{cfg: cfg, ctx: ctx, procs: make(map[string]*proc)}
	if cfg.IdleTTL > 0 {
		go r.gcLoop()
	}
	return r
}

// Acquire returns the live proc for sessionUUID. If none is alive it spawns
// one with opts, and reports fresh=true so the caller starts a reader for it.
func (r *Runtime) Acquire(sessionUUID string, opts SpawnOpts) (p *proc, fresh bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, ok := r.procs[sessionUUID]; ok {
		if p.alive() {
			p.touch()
			return p, false, nil
		}
		delete(r.procs, sessionUUID)
	}
	live := 0
	for _, q := range r.procs {
		if q.alive() {
			live++
		}
	}
	if r.cfg.MaxProcs > 0 && live >= r.cfg.MaxProcs {
		return nil, false, fmt.Errorf("claude: at max processes (%d)", r.cfg.MaxProcs)
	}
	p, err = r.spawn(sessionUUID, opts)
	return p, err == nil, err
}

// Get returns the live proc for sessionUUID without spawning.
func (r *Runtime) Get(sessionUUID string) *proc {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.procs[sessionUUID]; ok && p.alive() {
		return p
	}
	return nil
}

// spawn starts the process. Must be called with r.mu held.
func (r *Runtime) spawn(sessionUUID string, o SpawnOpts) (*proc, error) {
	// A missing cwd would fail exec with a misleading "fork/exec <binary>:
	// no such file or directory" that blames the binary. Say what is actually
	// wrong, as an invalid parameter, before anything is started.
	if o.Cwd != "" {
		if info, err := os.Stat(o.Cwd); err != nil {
			return nil, &backend.CwdNotExistError{Path: o.Cwd}
		} else if !info.IsDir() {
			return nil, backend.InvalidParams("working directory %q is not a directory", o.Cwd)
		}
	}
	ctx, cancel := context.WithCancel(r.ctx)

	args := []string{
		"--print",
		"--verbose", // required by --output-format stream-json
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--permission-prompt-tool=stdio", // can_use_tool arrives as control_request
		"--forward-subagent-text",        // a subagent's text arrives with parent_tool_use_id
	}
	if o.New {
		args = append(args, "--session-id", sessionUUID)
	} else {
		args = append(args, "--resume", sessionUUID)
	}
	for _, d := range o.AddDirs {
		args = append(args, "--add-dir", d)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if bypassAllowed {
		// Lets the session switch to bypassPermissions later; it does not start in it.
		args = append(args, "--allow-dangerously-skip-permissions")
	}
	if o.Mode != "" {
		args = append(args, "--permission-mode", o.Mode)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.SystemPr != "" {
		args = append(args, "--append-system-prompt", o.SystemPr)
	}

	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	cmd.Dir = o.Cwd
	cmd.Env = spawnEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	errTail := &tail{max: 4096}
	cmd.Stderr = errTail

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude spawn: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe() // must precede Start
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claude spawn: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("claude spawn: %w", err)
	}

	p := &proc{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		cancel:   cancel,
		stderr:   errTail,
		done:     make(chan struct{}),
		lastUsed: time.Now(),
	}
	r.procs[sessionUUID] = p
	return p, nil
}

// Kill terminates the session's process. The session's reader goroutine reaps it.
func (r *Runtime) Kill(sessionUUID string) {
	r.mu.Lock()
	p := r.procs[sessionUUID]
	delete(r.procs, sessionUUID)
	r.mu.Unlock()
	if p != nil {
		p.kill()
	}
}

func (p *proc) kill() {
	_ = p.stdin.Close() // EOF lets claude exit cleanly
	p.cancel()          // and SIGKILL via CommandContext if it does not
}

func (r *Runtime) gcLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.gc()
		}
	}
}

func (r *Runtime) gc() {
	cutoff := time.Now().Add(-r.cfg.IdleTTL)
	var victims []*proc
	r.mu.Lock()
	for id, p := range r.procs {
		p.mu.Lock()
		idle := p.lastUsed.Before(cutoff)
		p.mu.Unlock()
		if !p.alive() || (idle && !p.busy.Load()) {
			delete(r.procs, id)
			victims = append(victims, p)
		}
	}
	r.mu.Unlock()
	for _, p := range victims {
		p.kill()
	}
}

// Close kills every process.
func (r *Runtime) Close() {
	r.mu.Lock()
	all := r.procs
	r.procs = make(map[string]*proc)
	r.mu.Unlock()
	for _, p := range all {
		p.kill()
	}
}

// tail is an io.Writer that keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
