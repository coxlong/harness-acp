package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	claudebe "github.com/coxlong/harness-acp/internal/backend/claude"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Full stack: a real ACP v2 client, the Facade, the session registry and the
// real claude CLI. Costs a little per run:
//
//	CLAUDE_E2E=1 go test ./internal/acp -run E2E -v

// recorder is the client side. It records every session update, and answers
// permission requests by picking the "allow once" option.
type recorder struct {
	acp2.UnimplementedClient
	mu       sync.Mutex
	updates  []string // compact labels, in arrival order
	text     strings.Builder
	idles    int
	replayed bool
	asked    []string
	wake     chan struct{}
}

func newRecorder() *recorder { return &recorder{wake: make(chan struct{}, 1024)} }

func (r *recorder) SessionUpdate(_ context.Context, n *acp2.UpdateSessionNotification) error {
	r.mu.Lock()
	label := ""
	switch v := n.Update.Variant().(type) {
	case schema.SessionUpdateUserMessage:
		label = "user_message"
	case schema.SessionUpdateAgentMessageChunk:
		label = "agent_chunk"
		if t, ok := acp2.TextOf(v.Content); ok {
			r.text.WriteString(t)
		}
	case schema.SessionUpdateAgentMessage:
		label = "agent_message"
	case schema.SessionUpdateToolCallUpdate:
		label = "tool_call"
	case schema.SessionUpdateReplayComplete:
		label = "replay_complete"
		r.replayed = true
	case schema.SessionUpdateConfigOptionUpdate:
		label = "config_options"
	case schema.SessionUpdateStateUpdate:
		switch s := v.Value.Variant().(type) {
		case schema.StateUpdateRunning:
			label = "running"
		case schema.StateUpdateRequiresAction:
			label = "requires_action"
		case schema.StateUpdateIdle:
			label = "idle"
			if _, ok := s.Value.Variant().(schema.IdleStateUpdateEndTurn); ok {
				label = "idle:end_turn"
			}
			if _, ok := s.Value.Variant().(schema.IdleStateUpdateCancelled); ok {
				label = "idle:cancelled"
			}
			r.idles++
		}
	default:
		label = "other"
	}
	r.updates = append(r.updates, label)
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) RequestPermission(_ context.Context, req *acp2.RequestPermissionRequest) (*acp2.RequestPermissionResponse, error) {
	r.mu.Lock()
	r.asked = append(r.asked, req.Title)
	r.mu.Unlock()
	for _, o := range req.Options {
		if o.Kind == schema.PermissionOptionKindAllowOnce {
			return acp2.PermissionSelected(o.OptionID), nil
		}
	}
	return acp2.PermissionCancelled(), nil
}

func (r *recorder) waitIdles(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(120 * time.Second)
	for {
		r.mu.Lock()
		got := r.idles
		r.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-r.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for idle #%d; updates: %v", n, r.snapshot())
		}
	}
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.updates...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.updates, r.idles, r.replayed = nil, 0, false
	r.text.Reset()
	r.mu.Unlock()
}

type stack struct {
	be  backend.Backend
	reg *session.Registry
	dir string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	if os.Getenv("CLAUDE_E2E") == "" {
		t.Skip("set CLAUDE_E2E=1 to run against the real claude CLI")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	be := claudebe.NewBackend(claudebe.NewRuntime(ctx, claudebe.RuntimeConfig{}))
	t.Cleanup(func() { _ = be.Close() })
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &stack{be: be, reg: session.New(100), dir: dir}
}

// connect opens one client connection to the shared registry/backend, as a
// new websocket would.
func (s *stack) connect(t *testing.T) (*acp2.ClientSideConnection, *recorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := newRecorder()
	_, client := acp2.Pipe(ctx,
		func(c *acp2.AgentSideConnection) acp2.Agent {
			return NewFacade("claude", s.be, s.reg, []string{s.dir}, c)
		},
		func(*acp2.ClientSideConnection) acp2.Client { return rec },
	)
	if _, err := client.Initialize(ctx, &acp2.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}
	return client, rec
}

func prompt(sid acp2.SessionID, text string) *acp2.PromptRequest {
	return &acp2.PromptRequest{SessionID: sid, Prompt: []acp2.ContentBlock{acp2.TextBlock(text)}}
}

func has(updates []string, want ...string) bool {
	i := 0
	for _, u := range updates {
		if i < len(want) && u == want[i] {
			i++
		}
	}
	return i == len(want)
}

// Two prompts on one session: each turn ends with exactly one idle, and the
// output is not duplicated. (The old facade started a forwarding goroutine per
// prompt, so the second turn's events arrived twice.)
func TestE2E_TwoTurnsNoDuplicates(t *testing.T) {
	s := newStack(t)
	client, rec := s.connect(t)
	ctx := context.Background()

	ns, err := client.NewSession(ctx, &acp2.NewSessionRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	for turn := 1; turn <= 2; turn++ {
		rec.reset()
		if _, err := client.Prompt(ctx, prompt(ns.SessionID, "reply with exactly: pong")); err != nil {
			t.Fatal(err)
		}
		rec.waitIdles(t, 1)
		time.Sleep(500 * time.Millisecond) // a duplicate forwarder would deliver late copies
		got := rec.snapshot()
		t.Logf("turn %d: %v", turn, got)
		if !has(got, "user_message", "running", "agent_chunk", "idle:end_turn") {
			t.Errorf("turn %d: wrong order: %v", turn, got)
		}
		if n := count(got, "idle:end_turn"); n != 1 {
			t.Errorf("turn %d: %d idle events, want 1", turn, n)
		}
		if n := count(got, "user_message"); n != 1 {
			t.Errorf("turn %d: %d user_message events, want 1", turn, n)
		}
	}
}

func count(updates []string, label string) int {
	n := 0
	for _, u := range updates {
		if u == label {
			n++
		}
	}
	return n
}

// A tool that needs permission reaches the client as requires_action plus a
// permission request, and the turn finishes once it is allowed.
func TestE2E_Permission(t *testing.T) {
	s := newStack(t)
	client, rec := s.connect(t)
	ctx := context.Background()

	ns, err := client.NewSession(ctx, &acp2.NewSessionRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(s.dir, "x.txt")
	if _, err := client.Prompt(ctx, prompt(ns.SessionID, "Create the file "+target+" containing the single word hi, using the Write tool")); err != nil {
		t.Fatal(err)
	}
	rec.waitIdles(t, 1)
	got := rec.snapshot()
	t.Logf("%v asked=%v", got, rec.asked)
	if len(rec.asked) != 1 {
		t.Errorf("permission asked %d times, want 1", len(rec.asked))
	}
	if !has(got, "running", "requires_action", "running", "idle:end_turn") {
		t.Errorf("wrong state sequence: %v", got)
	}
	if b, err := os.ReadFile(target); err != nil || !strings.Contains(string(b), "hi") {
		t.Errorf("file not written: %v %q", err, b)
	}
}

// A second connection (a reconnecting app) resumes the session: it gets the
// history, replay_complete, an idle snapshot, and can prompt again.
func TestE2E_ResumeOnNewConnection(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	c1, rec1 := s.connect(t)
	ns, err := c1.NewSession(ctx, &acp2.NewSessionRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Prompt(ctx, prompt(ns.SessionID, "reply with exactly: pong")); err != nil {
		t.Fatal(err)
	}
	rec1.waitIdles(t, 1)

	c2, rec2 := s.connect(t)
	var from acp2.ReplayFrom
	from = acp2.NewReplayFrom(schema.ReplayFromStart{})
	if _, err := c2.ResumeSession(ctx, &acp2.ResumeSessionRequest{
		SessionID: ns.SessionID, Cwd: schema.AbsolutePath(s.dir), ReplayFrom: from,
	}); err != nil {
		t.Fatal(err)
	}
	got := rec2.snapshot()
	t.Logf("resume: %v", got)
	if !has(got, "user_message", "agent_message", "replay_complete", "idle") {
		t.Errorf("resume sequence wrong: %v", got)
	}
	if count(got, "idle:end_turn") != 0 {
		t.Errorf("the idle snapshot must carry no stop reason: %v", got)
	}

	rec2.reset()
	if _, err := c2.Prompt(ctx, prompt(ns.SessionID, "reply with exactly: pong2")); err != nil {
		t.Fatal(err)
	}
	rec2.waitIdles(t, 1)
	if !has(rec2.snapshot(), "user_message", "running", "agent_chunk", "idle:end_turn") {
		t.Errorf("prompt after resume: %v", rec2.snapshot())
	}
}

// A prompt on a session the connection has not attached to is refused: its
// output would have nowhere to go.
func TestE2E_PromptWithoutResumeRefused(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	c1, _ := s.connect(t)
	ns, err := c1.NewSession(ctx, &acp2.NewSessionRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := s.connect(t)
	if _, err := c2.Prompt(ctx, prompt(ns.SessionID, "hi")); err == nil {
		t.Error("prompt on an unattached connection should fail")
	}
}

// offeredSelect reports whether the select option id lists value.
func offeredSelect(opts []acp2.SessionConfigOption, id, value string) bool {
	for _, o := range opts {
		sel, ok := o.As[schema.SessionConfigOptionSelect]()
		if !ok || string(sel.ConfigID) != id {
			continue
		}
		list, err := sel.Options.As[[]schema.SessionConfigSelectOption]()
		if err != nil {
			return false
		}
		for _, v := range list {
			if string(v.Value) == value {
				return true
			}
		}
	}
	return false
}

func selectValue(opts []acp2.SessionConfigOption, id string) (string, bool) {
	for _, o := range opts {
		if sel, ok := o.As[schema.SessionConfigOptionSelect](); ok && string(sel.ConfigID) == id {
			return string(sel.CurrentValue), true
		}
	}
	return "", false
}

// A new session reports its options; changing one answers with all of them and
// tells the connection, and the change reaches the real process.
func TestE2E_ConfigOptions(t *testing.T) {
	s := newStack(t)
	client, rec := s.connect(t)
	ctx := context.Background()

	ns, err := client.NewSession(ctx, &acp2.NewSessionRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := selectValue(ns.ConfigOptions, "mode"); !ok || v != "default" {
		t.Errorf("mode = %q %v", v, ok)
	}
	if _, ok := selectValue(ns.ConfigOptions, "model"); !ok {
		t.Error("no model option")
	}
	if len(ns.AvailableCommands) == 0 {
		t.Error("no commands")
	}

	req := acp2.NewSetSessionConfigOptionRequest(schema.SetSessionConfigOptionRequestID{
		SessionID: ns.SessionID, ConfigID: "mode", Value: "plan",
	})
	resp, err := client.SetSessionConfigOption(ctx, &req)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := selectValue(resp.ConfigOptions, "mode"); v != "plan" {
		t.Errorf("mode after set = %q", v)
	}
	time.Sleep(200 * time.Millisecond)
	if !has(rec.snapshot(), "config_options") {
		t.Errorf("no config_option_update: %v", rec.snapshot())
	}

	// Auto and bypass are in the catalog, and the process accepts both. Running
	// as root, Claude Code refuses bypassPermissions, so only the modes this CLI
	// offers are required to be settable.
	for _, m := range []string{"auto", "bypassPermissions", "default"} {
		if !offeredSelect(ns.ConfigOptions, "mode", m) {
			t.Logf("mode %q is not offered by this CLI; not required", m)
			continue
		}
		r := acp2.NewSetSessionConfigOptionRequest(schema.SetSessionConfigOptionRequestID{
			SessionID: ns.SessionID, ConfigID: "mode", Value: schema.SessionConfigValueID(m),
		})
		resp, err := client.SetSessionConfigOption(ctx, &r)
		if err != nil {
			t.Fatalf("set mode %s: %v", m, err)
		}
		if v, _ := selectValue(resp.ConfigOptions, "mode"); v != m {
			t.Errorf("mode after set = %q, want %s", v, m)
		}
	}

	bad := acp2.NewSetSessionConfigOptionRequest(schema.SetSessionConfigOptionRequestID{
		SessionID: ns.SessionID, ConfigID: "mode", Value: "nonsense",
	})
	if _, err := client.SetSessionConfigOption(ctx, &bad); err == nil {
		t.Error("an unknown mode value was accepted")
	}

	// A model change is applied by the process, and effort follows the model.
	// The catalog is the CLI's: a gateway may not offer these values at all, so
	// only an offered value is required to be settable.
	beforeModel, _ := selectValue(ns.ConfigOptions, "model")
	for _, step := range []struct{ id, value string }{{"model", "sonnet"}, {"effort", "low"}} {
		if !offeredSelect(ns.ConfigOptions, step.id, step.value) {
			t.Logf("%s %q is not offered by this CLI; not required", step.id, step.value)
			continue
		}
		r := acp2.NewSetSessionConfigOptionRequest(schema.SetSessionConfigOptionRequestID{
			SessionID: ns.SessionID, ConfigID: schema.SessionConfigID(step.id), Value: schema.SessionConfigValueID(step.value),
		})
		resp, err := client.SetSessionConfigOption(ctx, &r)
		if err != nil {
			t.Fatalf("set %s=%s: %v", step.id, step.value, err)
		}
		if v, _ := selectValue(resp.ConfigOptions, step.id); v != step.value {
			t.Errorf("%s after set = %q, want %q", step.id, v, step.value)
		}
	}
	// Put the catalog back: the gateway's model names are not Claude's, and a
	// session left on a model the CLI cannot resolve never answers.
	if v, _ := selectValue(ns.ConfigOptions, "model"); v != beforeModel && beforeModel != "" {
		r := acp2.NewSetSessionConfigOptionRequest(schema.SetSessionConfigOptionRequestID{
			SessionID: ns.SessionID, ConfigID: "model", Value: schema.SessionConfigValueID(beforeModel),
		})
		if _, err := client.SetSessionConfigOption(ctx, &r); err != nil {
			t.Logf("restoring model %q: %v", beforeModel, err)
		}
	}

	// Options of a directory, without a session.
	o, err := client.GetSessionOptions(ctx, &acp2.SessionOptionsRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := selectValue(o.ConfigOptions, "mode"); v != "default" {
		t.Errorf("session/options mode = %q", v)
	}

	// A session still answers after its options changed. A fresh session, because
	// options belong to the session and a gateway's catalog may leave the first
	// one on a model the CLI cannot resolve.
	rec.reset()
	ns2, err := client.NewSession(ctx, &acp2.NewSessionRequest{Cwd: schema.AbsolutePath(s.dir)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prompt(ctx, prompt(ns2.SessionID, "reply with exactly: pong")); err != nil {
		t.Fatal(err)
	}
	rec.waitIdles(t, 1)
}
