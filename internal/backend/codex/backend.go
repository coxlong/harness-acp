// Package codex exposes the native Codex App Server as a harnessd backend.
package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/version"
)

type Backend struct {
	ctx       context.Context
	binary    string
	allowDirs []string

	mu      sync.Mutex
	process *process
	closed  bool
}

var _ backend.Backend = (*Backend)(nil)

func New(ctx context.Context, binary string, allowDirs []string) *Backend {
	return &Backend{ctx: ctx, binary: binary, allowDirs: allowDirs}
}

func (b *Backend) Name() string { return "codex" }

func (b *Backend) Probe(ctx context.Context) (backend.AgentInfo, error) {
	info := backend.AgentInfo{Name: b.Name(), DisplayName: "Codex"}
	out, err := exec.CommandContext(ctx, b.binary, "--version").Output()
	if err != nil {
		return info, err
	}
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		info.Version = fields[len(fields)-1]
	}
	_, err = b.connect(ctx)
	info.Available = err == nil
	return info, err
}

func (b *Backend) connect(ctx context.Context) (*process, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("Codex backend is closed")
	}
	if b.process != nil {
		select {
		case <-b.process.done:
		default:
			return b.process, nil
		}
	}
	p, err := spawn(b.ctx, b.binary)
	if err != nil {
		return nil, fmt.Errorf("start Codex app-server: %w", err)
	}
	init := object{
		"clientInfo":   object{"name": "harnessd", "title": "Harness", "version": version.Version},
		"capabilities": object{"experimentalApi": true},
	}
	if err = p.call(ctx, "initialize", init, nil); err == nil {
		err = p.notify(ctx, "initialized")
	}
	if err != nil {
		p.close()
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	b.process = p
	return p, nil
}

func (b *Backend) Close() error {
	b.mu.Lock()
	b.closed = true
	p := b.process
	b.mu.Unlock()
	if p != nil {
		p.close()
	}
	return nil
}

type object map[string]any

type thread struct {
	ID          string `json:"id"`
	Cwd         string `json:"cwd"`
	Name        string `json:"name"`
	Preview     string `json:"preview"`
	UpdatedAt   int64  `json:"updatedAt"`
	HistoryMode string `json:"historyMode"`
	Model       string `json:"model"`
	Provider    string `json:"modelProvider"`
	Effort      string `json:"reasoningEffort"`
	Turns       []turn `json:"turns"`
	ParentID    string `json:"parentThreadId"`
	Nickname    string `json:"agentNickname"`
	Role        string `json:"agentRole"`
	Status      struct {
		Type string `json:"type"`
	} `json:"status"`
}

type threadResponse struct {
	Thread   thread `json:"thread"`
	Model    string `json:"model"`
	Effort   string `json:"reasoningEffort"`
	Provider string `json:"modelProvider"`
	Profile  *struct {
		ID string `json:"id"`
	} `json:"activePermissionProfile"`
	Collaboration *struct {
		Mode string `json:"mode"`
	} `json:"collaborationMode"`
	Cwd string `json:"cwd"`
}

func (b *Backend) NewSession(ctx context.Context, p backend.NewSessionParams) (backend.SessionHandle, error) {
	params, err := b.workspace(p.Cwd, p.AdditionalDirectories)
	if err != nil {
		return nil, err
	}
	if p.Model != "" {
		params["model"] = p.Model
	}
	if p.Instructions != "" {
		params["developerInstructions"] = p.Instructions
	}
	proc, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.applySetupConfig(ctx, proc, p.Cwd, nil, p.Config, params); err != nil {
		return nil, err
	}
	return b.open(ctx, "thread/start", params, p.Config)
}

func (b *Backend) ResumeSession(ctx context.Context, id string, p backend.ResumeParams) (backend.SessionHandle, error) {
	proc, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	if s := proc.find(id); s != nil {
		return s, nil
	}
	t, err := b.readThread(ctx, proc, id, false)
	if err != nil {
		return nil, err
	}
	if t.ParentID != "" {
		return nil, &backend.SessionNotFoundError{ID: id}
	}
	params, err := b.workspace(t.Cwd, p.AdditionalDirectories)
	if err != nil {
		return nil, err
	}
	params["threadId"] = id
	params["excludeTurns"] = true
	if err := b.applySetupConfig(ctx, proc, t.Cwd, &t, p.Config, params); err != nil {
		return nil, err
	}
	return b.open(ctx, "thread/resume", params, p.Config)
}

func (b *Backend) open(ctx context.Context, method string, params object, initial []backend.ConfigSelection) (backend.SessionHandle, error) {
	p, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	var result threadResponse
	if err := p.call(ctx, method, params, &result); err != nil {
		id, _ := params["threadId"].(string)
		return nil, threadError(id, err)
	}
	if result.Thread.ID == "" {
		return nil, errors.New("Codex returned a thread without an ID")
	}
	s := newSession(p, result)
	s.fresh = method == "thread/start"
	s.instructions, _ = params["developerInstructions"].(string)
	for _, selected := range initial {
		if selected.ID == "collaboration_mode" && collaborationMode(selected.Value) {
			s.mode, s.sendMode = selected.Value, true
		}
	}
	active, err := p.put(s)
	if err != nil {
		return nil, err
	}
	if active == s {
		models := p.models(ctx, result.Provider)
		profiles := p.profiles(ctx, result.Thread.Cwd)
		s.mu.Lock()
		s.models, s.profiles = models, profiles
		if method != "thread/start" {
			s.watchTerminals()
		}
		s.mu.Unlock()
	}
	return active, nil
}

func (p *process) find(id string) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[id]
}

func (b *Backend) workspace(cwd string, dirs []string) (object, error) {
	if err := b.checkDir(cwd); err != nil {
		return nil, err
	}
	params := object{"cwd": cwd}
	if len(dirs) > 0 {
		for _, dir := range dirs {
			if err := b.checkDir(dir); err != nil {
				return nil, err
			}
		}
		params["runtimeWorkspaceRoots"] = append([]string{cwd}, dirs...)
	}
	return params, nil
}

func (b *Backend) checkDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return backend.InvalidParams("working directory %q must be an absolute path", dir)
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return &backend.CwdNotExistError{Path: dir}
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return backend.InvalidParams("%q is not a directory", dir)
	}
	if !b.allowed(dir) {
		return backend.InvalidParams("working directory %q is not under an allowed root", dir)
	}
	return nil
}

func (b *Backend) allowed(dir string) bool {
	if len(b.allowDirs) == 0 {
		return true
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	for _, root := range b.allowDirs {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolved, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func (b *Backend) readThread(ctx context.Context, p *process, id string, turns bool) (thread, error) {
	var result threadResponse
	err := p.call(ctx, "thread/read", object{"threadId": id, "includeTurns": turns}, &result)
	if err != nil {
		return thread{}, threadError(id, err)
	}
	if !b.allowed(result.Thread.Cwd) {
		return thread{}, &backend.SessionNotFoundError{ID: id}
	}
	return result.Thread, nil
}

func threadError(id string, err error) error {
	var rpc *rpcError
	if !errors.As(err, &rpc) {
		return err
	}
	m := strings.ToLower(rpc.Message)
	if id != "" && (strings.Contains(m, "thread not found") || strings.Contains(m, "thread not loaded") || strings.Contains(m, "unknown thread") ||
		strings.Contains(m, "no rollout found") || strings.Contains(m, "invalid thread id") ||
		strings.Contains(m, "thread id") && strings.Contains(m, "invalid")) {
		return &backend.SessionNotFoundError{ID: id}
	}
	if rpc.Code == -32602 {
		return backend.InvalidParams("%s", rpc.Message)
	}
	return err
}

// ListSessionsPage passes the native opaque cursor through the ACP facade.
func (b *Backend) ListSessionsPage(ctx context.Context, f backend.ListFilter, cursor *string) ([]backend.SessionInfo, *string, error) {
	p, err := b.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	if f.Archived == "include" {
		return b.listCombined(ctx, p, f, cursor)
	}
	return b.listNative(ctx, p, f.Cwd, f.Limit, f.Archived == "only", cursor)
}

func (b *Backend) listNative(ctx context.Context, p *process, cwd string, limit int, archived bool, cursor *string, ids ...string) ([]backend.SessionInfo, *string, error) {
	params := object{
		"archived": archived, "sortKey": "updated_at", "cursor": cursor,
		"modelProviders": []string{},
		"sourceKinds":    []string{"cli", "vscode", "appServer", "unknown"},
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if limit > 0 {
		params["limit"] = limit
	}
	if len(ids) > 0 {
		params["includedThreadIds"] = ids
	}
	var result struct {
		Data       []thread `json:"data"`
		NextCursor *string  `json:"nextCursor"`
	}
	if err := p.call(ctx, "thread/list", params, &result); err != nil {
		return nil, nil, threadError("", err)
	}
	out := make([]backend.SessionInfo, 0, len(result.Data))
	for _, t := range result.Data {
		if t.ParentID == "" && b.allowed(t.Cwd) {
			out = append(out, backend.SessionInfo{
				ID: t.ID, Cwd: t.Cwd, Title: threadTitle(t), UpdatedAt: time.Unix(t.UpdatedAt, 0), Archived: &archived,
			})
		}
	}
	return out, result.NextCursor, nil
}

func (b *Backend) ListSessions(ctx context.Context, f backend.ListFilter) ([]backend.SessionInfo, bool, error) {
	var all []backend.SessionInfo
	var cursor *string
	limit := f.Limit
	f.Limit = 100
	for {
		page, next, err := b.ListSessionsPage(ctx, f, cursor)
		if err != nil {
			return nil, false, err
		}
		skip := min(max(f.Offset, 0), len(page))
		f.Offset -= skip
		all = append(all, page[skip:]...)
		if limit > 0 && len(all) >= limit {
			return all[:limit], len(all) > limit || next != nil, nil
		}
		if next == nil {
			return all, false, nil
		}
		if cursor != nil && *next == *cursor {
			return nil, false, errors.New("Codex returned a repeated session cursor")
		}
		cursor = next
	}
}

func threadTitle(t thread) string {
	if t.Name != "" {
		return t.Name
	}
	return t.Preview
}

func (b *Backend) DeleteSession(ctx context.Context, id string) error {
	p, err := b.connect(ctx)
	if err != nil {
		return err
	}
	t, err := b.readThread(ctx, p, id, false)
	if err != nil {
		var missing *backend.SessionNotFoundError
		if errors.As(err, &missing) {
			return nil
		}
		return err
	}
	if t.ParentID != "" {
		return backend.InvalidParams("cannot delete a Codex subagent session")
	}
	err = threadError(id, p.call(ctx, "thread/delete", object{"threadId": id}, nil))
	var missing *backend.SessionNotFoundError
	if errors.As(err, &missing) {
		return nil
	}
	return err
}
