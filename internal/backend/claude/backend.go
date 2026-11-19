package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// listLimit caps one session/list response, as claude-agent-acp does.
const listLimit = 1000

// Backend is the process-level singleton for the claude agent.
type Backend struct {
	rt *Runtime
}

var _ backend.Backend = (*Backend)(nil)

// NewBackend creates the claude backend over rt.
func NewBackend(rt *Runtime) *Backend { return &Backend{rt: rt} }

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

// NewSession allocates a session. The process starts at the first prompt, with
// --session-id, so a session that is never prompted costs no process.
func (b *Backend) NewSession(ctx context.Context, p backend.NewSessionParams) (backend.SessionHandle, error) {
	uuid := newUUID()
	return NewSession(uuid, b.rt, SpawnOpts{
		Cwd:      p.Cwd,
		AddDirs:  p.AdditionalDirectories,
		New:      true,
		Model:    p.Model,
		SystemPr: p.Instructions,
	}), nil
}

// ResumeSession attaches to an existing session. The process starts at the
// next prompt, with --resume. Claude resolves --resume against the project
// directory of its cwd, so the cwd recorded in the transcript wins over the
// request's: a client may reopen a session from another directory.
func (b *Backend) ResumeSession(ctx context.Context, id string, p backend.ResumeParams) (backend.SessionHandle, error) {
	path, err := findTranscript(id)
	if err != nil {
		return nil, err
	}
	cwd := p.Cwd
	if meta, err := readHead(path); err == nil && meta.cwd != "" {
		cwd = meta.cwd
	}
	return NewSession(id, b.rt, SpawnOpts{Cwd: cwd, AddDirs: p.AdditionalDirectories}), nil
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

type headMeta struct {
	cwd   string
	title string
}

// readHead reads the cwd and the title (the first real user prompt) from the
// start of a transcript.
func readHead(path string) (headMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return headMeta{}, err
	}
	defer f.Close()
	var meta headMeta
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 0; sc.Scan() && n < 200; n++ {
		var l transcriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if meta.cwd == "" {
			meta.cwd = l.Cwd
		}
		if meta.title == "" && l.Type == "user" && !l.IsSidechain && !l.IsMeta {
			if text := userText(l.Message); text != "" && !hiddenUserText(text) {
				meta.title = truncate(firstLine(text), 80)
			}
		}
		if meta.cwd != "" && meta.title != "" {
			break
		}
	}
	return meta, sc.Err()
}

// ListSessions lists transcripts, newest first. The transcripts are the only
// store: harnessd keeps no session index of its own.
func (b *Backend) ListSessions(ctx context.Context, f backend.ListFilter) ([]backend.SessionInfo, string, error) {
	paths, _ := filepath.Glob(filepath.Join(configDir(), "projects", "*", "*.jsonl"))
	type item struct {
		info backend.SessionInfo
		mod  int64
	}
	var items []item
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		meta, err := readHead(p)
		if err != nil || meta.cwd == "" {
			continue // an empty or foreign file: not a resumable session
		}
		if f.Cwd != "" && meta.cwd != f.Cwd {
			continue
		}
		items = append(items, item{
			info: backend.SessionInfo{
				ID:    strings.TrimSuffix(filepath.Base(p), ".jsonl"),
				Cwd:   meta.cwd,
				Title: meta.title,
			},
			mod: st.ModTime().UnixNano(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod > items[j].mod })
	if len(items) > listLimit {
		items = items[:listLimit]
	}
	out := make([]backend.SessionInfo, len(items))
	for i, it := range items {
		out[i] = it.info
	}
	return out, "", nil
}

// transcriptLine is one JSONL line. Only user and assistant lines are
// messages; the file also holds queue-operation, attachment, api-request-blob
// and other bookkeeping lines, some of which carry a `message` that looks
// like one.
type transcriptLine struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	Cwd         string          `json:"cwd"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	Message     json.RawMessage `json:"message"`
}

// History replays a session from its transcript as entity-level updates, as
// the live session sent them. Claude writes each content block of an API
// message as its own line, all with the same message id, so consecutive
// assistant lines of one id are merged into one message.
func (b *Backend) History(ctx context.Context, id string) ([]acp2.SessionUpdate, error) {
	path, err := findTranscript(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := &history{open: map[string]bool{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var l transcriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.IsSidechain || l.IsMeta || (l.Type != "user" && l.Type != "assistant") {
			continue
		}
		switch l.Type {
		case "user":
			h.user(&l)
		case "assistant":
			h.assistant(&l)
		}
	}
	h.flush()
	// A call the transcript gives no result (it ran when Claude Code was
	// killed) ends failed, as v2 asks of replayed work that no longer runs.
	for toolID := range h.open {
		h.out = append(h.out, schema.NewSessionUpdate(schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(toolID),
			Status:     optional.Of(schema.ToolCallStatusFailed),
		}))
	}
	return h.out, sc.Err()
}

type history struct {
	out  []acp2.SessionUpdate
	open map[string]bool // tool calls without a result yet

	// the assistant message being merged
	curID    string
	text     []acp2.ContentBlock
	thoughts []acp2.ContentBlock
}

// flush emits the merged assistant message, if any.
func (h *history) flush() {
	if h.curID == "" {
		return
	}
	if len(h.thoughts) > 0 {
		h.out = append(h.out, schema.NewSessionUpdate(schema.SessionUpdateAgentThought{
			MessageID: acp2.MessageID(h.curID + "-thought"),
			Content:   optional.Of(h.thoughts),
		}))
	}
	if len(h.text) > 0 {
		h.out = append(h.out, schema.NewSessionUpdate(schema.SessionUpdateAgentMessage{
			MessageID: acp2.MessageID(h.curID),
			Content:   optional.Of(h.text),
		}))
	}
	h.curID, h.text, h.thoughts = "", nil, nil
}

func (h *history) assistant(l *transcriptLine) {
	var m message
	if json.Unmarshal(l.Message, &m) != nil || m.Model == "<synthetic>" {
		return
	}
	id := firstNonEmpty(m.ID, l.UUID)
	if id != h.curID {
		h.flush()
		h.curID = id
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				h.text = append(h.text, acp2.TextBlock(b.Text))
			}
		case "thinking":
			if b.Thinking != "" {
				h.thoughts = append(h.thoughts, acp2.TextBlock(b.Thinking))
			}
		case "tool_use", "server_tool_use":
			var input map[string]any
			_ = json.Unmarshal(b.Input, &input)
			// The message's text goes out before the tool call it leads to.
			h.flushKeepID()
			for _, ev := range toolCall(b.ID, toolTitle(b.Name, input), toolKind(b.Name),
				schema.ToolCallStatusInProgress, b.Input) {
				h.out = append(h.out, ev.Update)
			}
			h.open[b.ID] = true
		}
	}
}

// flushKeepID emits the text gathered so far but keeps merging into the same
// message id: a later line of the same API message extends it.
func (h *history) flushKeepID() {
	id := h.curID
	h.flush()
	h.curID = id
}

func (h *history) user(l *transcriptLine) {
	h.flush()
	var m message
	if json.Unmarshal(l.Message, &m) != nil {
		return
	}
	// A plain string, or blocks: text is the user's prompt, tool_result is a
	// tool's output.
	if text := userText(l.Message); text != "" {
		if hiddenUserText(text) {
			return
		}
		h.out = append(h.out, schema.NewSessionUpdate(schema.SessionUpdateUserMessage{
			MessageID: acp2.MessageID(l.UUID),
			Content:   optional.Of([]acp2.ContentBlock{acp2.TextBlock(text)}),
		}))
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		status := schema.ToolCallStatusCompleted
		if b.IsError {
			status = schema.ToolCallStatusFailed
		}
		u := schema.SessionUpdateToolCallUpdate{
			ToolCallID: acp2.ToolCallID(b.ToolUseID),
			Status:     optional.Of(status),
		}
		if text := resultText(b.Content); text != "" {
			u.Content = optional.Of([]acp2.ToolCallContent{acp2.ToolText(text)})
		}
		h.out = append(h.out, schema.NewSessionUpdate(u))
		delete(h.open, b.ToolUseID)
	}
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
		strings.HasPrefix(t, "<local-command") ||
		strings.HasPrefix(t, "<command-name>") ||
		strings.HasPrefix(t, "<system-reminder>")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
