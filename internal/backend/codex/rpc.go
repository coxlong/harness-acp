package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const rpcTimeout = 20 * time.Second

// Codex uses newline-delimited JSON-RPC and omits the jsonrpc member.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

type process struct {
	cancel            context.CancelFunc
	stdin             io.WriteCloser
	done              chan struct{}
	exited            chan struct{}
	writes            chan []byte
	permissionTimeout time.Duration

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan message
	sessions map[string]*Session
	children map[string]*Session
	failure  error
}

func spawn(ctx context.Context, binary string) (*process, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, binary, "app-server", "--enable", "default_mode_request_user_input")
	// Gateway credentials are used only by harnessd. Provider credentials
	// still come from the service account's environment.
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "HARNESSD_TOKEN=")
	})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, err
	}
	// Keep Codex logs away from the ACP stdio channel and avoid logging its
	// environment, requests, or provider diagnostics.
	if err := cmd.Start(); err != nil {
		cancel()
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	p := &process{
		cancel: cancel, stdin: stdin, done: make(chan struct{}), exited: make(chan struct{}),
		writes: make(chan []byte, 128), pending: make(map[uint64]chan message),
		sessions: make(map[string]*Session), children: make(map[string]*Session),
	}
	go p.writeLoop()
	go func() {
		defer close(p.exited)
		err := p.readLoop(stdout)
		p.fail(err)
		_ = cmd.Wait()
	}()
	return p, nil
}

func (p *process) fail(err error) {
	p.mu.Lock()
	if p.failure != nil {
		p.mu.Unlock()
		return
	}
	p.failure = fmt.Errorf("Codex app-server stopped: %w", err)
	sessions := p.sessions
	p.sessions = nil
	p.children = nil
	close(p.done)
	p.mu.Unlock()
	p.cancel()
	_ = p.stdin.Close()
	for _, s := range sessions {
		s.end(p.failure)
	}
}

func (p *process) close() {
	p.fail(context.Canceled)
	<-p.exited
}

func (p *process) writeLoop() {
	for {
		select {
		case <-p.done:
			return
		case line := <-p.writes:
			if _, err := p.stdin.Write(line); err != nil {
				p.fail(err)
				return
			}
		}
	}
}

func (p *process) queue(ctx context.Context, m message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	select {
	case <-p.done:
		return p.failure
	default:
	}
	select {
	case <-p.done:
		return p.failure
	case <-ctx.Done():
		return ctx.Err()
	case p.writes <- append(raw, '\n'):
		return nil
	}
}

func (p *process) notify(ctx context.Context, method string) error {
	return p.queue(ctx, message{Method: method})
}

func (p *process) call(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan message, 1)
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()
	if err := p.queue(ctx, message{ID: json.RawMessage(fmt.Appendf(nil, "%d", id)), Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("Codex %s: %w", method, ctx.Err())
	case <-p.done:
		return p.failure
	case reply := <-ch:
		if reply.Error != nil {
			return reply.Error
		}
		if result != nil {
			if err := json.Unmarshal(reply.Result, result); err != nil {
				return fmt.Errorf("decode Codex %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (p *process) readLoop(stdout io.Reader) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("decode app-server message: %w", err)
		}
		if m.Method == "" {
			p.deliverReply(m)
		} else {
			p.dispatch(m)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (p *process) deliverReply(m message) {
	var id uint64
	if json.Unmarshal(m.ID, &id) != nil || id == 0 {
		return
	}
	p.mu.Lock()
	ch := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (p *process) dispatch(m message) {
	if m.Method == "skills/changed" {
		p.mu.Lock()
		roots := make([]*Session, 0, len(p.sessions))
		for _, s := range p.sessions {
			roots = append(roots, s)
		}
		p.mu.Unlock()
		for _, s := range roots {
			s.scheduleSkillsRefresh()
		}
		return
	}
	var key struct {
		ThreadID string `json:"threadId"`
		Thread   thread `json:"thread"`
	}
	_ = json.Unmarshal(m.Params, &key)
	p.mu.Lock()
	s := p.sessions[key.ThreadID]
	if s == nil {
		s = p.children[key.ThreadID]
	}
	if s == nil && key.Thread.ID != "" {
		s = p.sessions[key.Thread.ID]
		if s == nil {
			s = p.children[key.Thread.ID]
		}
	}
	parent := p.sessions[key.Thread.ParentID]
	if parent == nil {
		parent = p.children[key.Thread.ParentID]
	}
	p.mu.Unlock()
	if m.Method == "thread/started" && parent != nil && key.Thread.ID != "" {
		parent.mu.Lock()
		if child := parent.addChild(key.Thread.ID, key.Thread); child != nil {
			s = child
		}
		parent.mu.Unlock()
	}
	if s != nil {
		s.receive(m)
	} else if len(m.ID) > 0 {
		p.respond(m.ID, nil, &rpcError{Code: -32601, Message: "Request is not supported for this session"})
	}
}

func (p *process) respond(id json.RawMessage, result any, rpcErr *rpcError) {
	m := message{ID: id, Error: rpcErr}
	if rpcErr == nil {
		m.Result, _ = json.Marshal(result)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	// Server requests can arrive while a call is waiting for its response.
	// Keep the reader independent of the pipe writer.
	select {
	case <-p.done:
	case p.writes <- append(raw, '\n'):
	default:
		log.Printf("Codex app-server write queue exhausted")
		go p.fail(errors.New("write queue exhausted"))
	}
}

func (p *process) put(s *Session) (*Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return nil, p.failure
	}
	if existing := p.sessions[s.id]; existing != nil {
		return existing, nil
	}
	if p.children[s.id] != nil {
		return nil, fmt.Errorf("Codex thread %s belongs to a subagent", s.id)
	}
	p.sessions[s.id] = s
	return s, nil
}

func (p *process) remove(s *Session) {
	p.mu.Lock()
	if p.sessions[s.id] == s {
		delete(p.sessions, s.id)
	}
	if p.children[s.id] == s {
		delete(p.children, s.id)
	}
	p.mu.Unlock()
}
