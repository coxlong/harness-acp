package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
)

// ListSessions lists transcripts, newest first. The transcripts are the only
// store: harnessd keeps no session index of its own.
//
// It follows what claude-agent-acp gets from the SDK's listSessions: newest
// first by modification time, one entry per session id, and the sessions the
// SDK finds no title or no directory for are left out. A page is built from
// the front of that order, so reading stops once it is full.
func (b *Backend) ListSessions(ctx context.Context, f backend.ListFilter) ([]backend.SessionInfo, bool, error) {
	paths, _ := filepath.Glob(filepath.Join(configDir(), "projects", "*", "*.jsonl"))
	type file struct {
		path string
		id   string
		mod  time.Time
	}
	files := make([]file, 0, len(paths))
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		files = append(files, file{p, strings.TrimSuffix(filepath.Base(p), ".jsonl"), st.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].id > files[j].id
	})

	wantCwd := ""
	if f.Cwd != "" {
		wantCwd = filepath.Clean(f.Cwd)
	}
	want := -1 // one more than the page tells whether another page follows
	if f.Limit > 0 {
		want = f.Offset + f.Limit + 1
	}

	var (
		out     []backend.SessionInfo
		seen    = map[string]bool{}
		skipped int
	)
	const chunk = 32
	for i := 0; i < len(files) && (want < 0 || len(out) < want); i += chunk {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		end := min(i+chunk, len(files))
		metas := make([]*transcriptMeta, end-i)
		var wg sync.WaitGroup
		for j := i; j < end; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if m, err := scanTranscript(files[j].path); err == nil && m.title() != "" && m.cwd != "" {
					metas[j-i] = &m
				}
			}()
		}
		wg.Wait()
		for j := i; j < end; j++ {
			m := metas[j-i]
			if m == nil || seen[files[j].id] {
				continue
			}
			if wantCwd != "" && filepath.Clean(m.cwd) != wantCwd {
				continue
			}
			seen[files[j].id] = true
			if skipped < f.Offset {
				skipped++
				continue
			}
			out = append(out, backend.SessionInfo{
				ID:        files[j].id,
				Cwd:       m.cwd,
				Title:     sanitizeTitle(m.title()),
				UpdatedAt: files[j].mod,
			})
			if want >= 0 && len(out) >= f.Limit+1 {
				break
			}
		}
		if want >= 0 && len(out) >= f.Limit+1 {
			break
		}
	}
	more := false
	if f.Limit > 0 && len(out) > f.Limit {
		out, more = out[:f.Limit], true
	}
	return out, more, nil
}

// transcriptMeta is what a listing reads of one transcript, folded the way the
// SDK folds it: the first entry that has a value fixes it, or, for the titles,
// the last one that has it wins.
type transcriptMeta struct {
	sidechain      bool // the first entry belongs to a subagent: not a session
	cwd            string
	customTitle    string // the user's /rename
	aiTitle        string
	lastPrompt     string
	summaryHint    string
	firstPrompt    string
	firstLocked    bool
	commandAsFirst string // a slash command, when no prompt came before it
}

// title is the session's display text, in the SDK's order: the name the user
// set, the generated title, the last prompt, a summary, the first prompt.
func (m *transcriptMeta) title() string {
	first := m.commandAsFirst
	if m.firstLocked {
		first = m.firstPrompt
	}
	for _, s := range []string{m.customTitle, m.aiTitle, m.lastPrompt, m.summaryHint, first} {
		if s != "" {
			return s
		}
	}
	return ""
}

// metaLine is the part of a transcript line a listing reads. A field is a
// pointer where the SDK tests for "a string": an empty one still counts.
type metaLine struct {
	Type             string          `json:"type"`
	Cwd              string          `json:"cwd"`
	RelocatedCwd     string          `json:"relocatedCwd"`
	IsSidechain      bool            `json:"isSidechain"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
	CustomTitle      *string         `json:"customTitle"`
	AITitle          *string         `json:"aiTitle"`
	LastPrompt       *string         `json:"lastPrompt"`
	Summary          *string         `json:"summary"`
}

// Lines worth decoding once the directory and the first prompt are known:
// those that can carry a title or move the directory.
var titleKeys = [][]byte{
	[]byte(`"customTitle"`), []byte(`"aiTitle"`), []byte(`"lastPrompt"`),
	[]byte(`"summary"`), []byte(`"relocatedCwd"`),
}

// scanTranscript reads a transcript's listing metadata. Transcripts hold huge
// lines (request blobs), so a line is decoded only when it can matter.
func scanTranscript(path string) (transcriptMeta, error) {
	var m transcriptMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	first := true
	cwdSet := false
	for sc.Scan() {
		raw := sc.Bytes()
		needHead := first || !cwdSet || !m.firstLocked
		if !needHead && !containsAny(raw, titleKeys) {
			continue
		}
		var l metaLine
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		if first {
			m.sidechain = l.IsSidechain
			first = false
		}
		if !cwdSet && l.Cwd != "" {
			m.cwd, cwdSet = l.Cwd, true
		}
		if !m.firstLocked {
			m.foldFirstPrompt(&l)
		}
		setIf(&m.customTitle, l.CustomTitle)
		setIf(&m.aiTitle, l.AITitle)
		setIf(&m.lastPrompt, l.LastPrompt)
		setIf(&m.summaryHint, l.Summary)
		if l.Type == "relocated" && l.RelocatedCwd != "" {
			m.cwd = l.RelocatedCwd
		}
	}
	if m.sidechain {
		m.customTitle, m.aiTitle, m.lastPrompt, m.summaryHint = "", "", "", ""
		m.firstPrompt, m.firstLocked, m.commandAsFirst = "", false, ""
	}
	return m, sc.Err()
}

func containsAny(b []byte, keys [][]byte) bool {
	for _, k := range keys {
		if bytes.Contains(b, k) {
			return true
		}
	}
	return false
}

func setIf(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

var (
	// A text that opens with a tag, or the interrupt marker, is not a prompt.
	notPromptRe = regexp.MustCompile(`^(?:\s*<[a-z][\w-]*[\s>]|\[Request interrupted by user[^\]]*\])`)
	commandRe   = regexp.MustCompile(`<command-name>(.*?)</command-name>`)
	bashInputRe = regexp.MustCompile(`(?s)<bash-input>(.*?)</bash-input>`)
)

// foldFirstPrompt reads the session's first prompt from a user line, as the
// SDK does: meta and summary lines and tool results are no prompt; a slash
// command is remembered as a fallback; a shell input shows as "! cmd".
func (m *transcriptMeta) foldFirstPrompt(l *metaLine) {
	if l.Type != "user" || l.IsMeta || l.IsCompactSummary || len(l.Message) == 0 {
		return
	}
	var msg message
	if json.Unmarshal(l.Message, &msg) != nil {
		return
	}
	var texts []string
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		texts = append(texts, s)
	} else {
		var blocks []block
		if json.Unmarshal(msg.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "tool_result" {
					return
				}
				if b.Type == "text" {
					texts = append(texts, b.Text)
				}
			}
		}
	}
	for _, t := range texts {
		c := strings.TrimSpace(strings.ReplaceAll(t, "\n", " "))
		if c == "" {
			continue
		}
		if g := commandRe.FindStringSubmatch(c); g != nil {
			if m.commandAsFirst == "" {
				m.commandAsFirst = g[1]
			}
			continue
		}
		if g := bashInputRe.FindStringSubmatch(c); g != nil {
			m.firstPrompt, m.firstLocked = "! "+strings.TrimSpace(g[1]), true
			return
		}
		if notPromptRe.MatchString(c) {
			continue
		}
		if r := []rune(c); len(r) > 200 {
			c = strings.TrimSpace(string(r[:200])) + "…"
		}
		m.firstPrompt, m.firstLocked = c, true
		return
	}
}

// maxTitle is the longest title session/list reports, as claude-agent-acp does.
const maxTitle = 256

var spaceRe = regexp.MustCompile(`\s+`)

// sanitizeTitle makes a title one line: whitespace collapsed, cut at maxTitle
// with an ellipsis.
func sanitizeTitle(s string) string {
	// Only the start of a long text can matter, and a prompt can be megabytes.
	if len(s) > 4096 {
		s = strings.ToValidUTF8(s[:4096], "")
	}
	s = strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > maxTitle {
		return string(r[:maxTitle-1]) + "…"
	}
	return s
}
