package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// What a tool result shows the client. Edit and Write results carry the change
// as a diff, rebuilt from Claude Code's structured result; everything else
// shows the text (or image) the model saw. Mirrors claude-agent-acp's
// tool-calls/content.ts and diff.ts.

// diffResult is the `tool_use_result` of Edit and Write.
type diffResult struct {
	Type            string      `json:"type"` // Write only: "create" or "update"
	FilePath        string      `json:"filePath"`
	Content         *string     `json:"content"`      // Write only: what was written
	OriginalFile    *string     `json:"originalFile"` // null on a create or a very large file
	OldString       *string     `json:"oldString"`    // Edit only
	StructuredPatch []patchHunk `json:"structuredPatch"`
}

type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

const noNewline = `\ No newline at end of file`

// maxPatchText bounds the file text that a created file's patch repeats.
const maxPatchText = 1 << 20

// fileDiff builds the content and locations that show an Edit or Write result.
// ok is false when the result names no file, or changed nothing.
func fileDiff(raw json.RawMessage) (content []acp2.ToolCallContent, locs []schema.ToolCallLocation, ok bool) {
	var r diffResult
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil || r.FilePath == "" {
		return nil, nil, false
	}
	path := schema.AbsolutePath(r.FilePath)
	if r.Type == "create" {
		var hunks []patchHunk
		if r.Content != nil && patchable(*r.Content) {
			if h, ok := wholeFileHunk(*r.Content); ok {
				hunks = []patchHunk{h}
			}
		}
		return []acp2.ToolCallContent{diffContent(r.FilePath, true, hunks)},
			[]schema.ToolCallLocation{{Path: path}}, true
	}
	// An Edit with an empty old_string reports the same result for a created
	// file and a filled empty one: it gets no diff.
	if r.Type == "" && r.OldString != nil && *r.OldString == "" && r.OriginalFile != nil && *r.OriginalFile == "" {
		return nil, nil, false
	}
	// A Write whose content is the previous text changed nothing.
	if r.Type == "update" && r.Content != nil && r.OriginalFile != nil && *r.Content == *r.OriginalFile {
		return nil, nil, false
	}
	content = []acp2.ToolCallContent{diffContent(r.FilePath, false, r.StructuredPatch)}
	if len(r.StructuredPatch) == 0 {
		return content, []schema.ToolCallLocation{{Path: path}}, true
	}
	for _, h := range r.StructuredPatch {
		line := uint32(max(h.NewStart, 0))
		locs = append(locs, schema.ToolCallLocation{Path: path, Line: &line})
	}
	return content, locs, true
}

func diffContent(path string, create bool, hunks []patchHunk) acp2.ToolCallContent {
	p := schema.AbsolutePath(path)
	ft := schema.DiffFileTypeText
	var change schema.DiffChange
	if create {
		change = schema.NewDiffChange(schema.DiffChangeAdd{Path: p, FileType: &ft})
	} else {
		change = schema.NewDiffChange(schema.DiffChangeModify{Path: p, FileType: &ft})
	}
	d := schema.ToolCallContentDiff{Changes: []schema.DiffChange{change}}
	if len(hunks) > 0 {
		d.Patch = &schema.DiffPatch{Format: schema.DiffPatchFormatGitPatch, Text: gitPatch(path, create, hunks)}
	}
	return schema.NewToolCallContent(d)
}

func patchable(s string) bool {
	return len(s) <= maxPatchText && !strings.ContainsAny(s, "\x00\r")
}

// wholeFileHunk is the hunk that adds every line of text to an empty file.
func wholeFileHunk(text string) (patchHunk, bool) {
	if text == "" {
		return patchHunk{}, false
	}
	terminated := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	h := patchHunk{OldStart: 1, NewStart: 1, NewLines: len(lines)}
	for _, l := range lines {
		h.Lines = append(h.Lines, "+"+l)
	}
	if !terminated {
		h.Lines = append(h.Lines, noNewline)
	}
	return h, true
}

// gitPatch writes one file's patch in git's form, with the file's own absolute
// path (the form an ACP v2 git_patch takes).
func gitPatch(path string, create bool, hunks []patchHunk) string {
	name := quoteGitPath(path)
	tab := ""
	if strings.Contains(path, " ") {
		tab = "\t" // git ends a name that holds a space with a tab, for GNU patch
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", name, name)
	if create {
		b.WriteString("new file mode 100644\n--- /dev/null\n")
	} else {
		fmt.Fprintf(&b, "--- %s%s\n", name, tab)
	}
	fmt.Fprintf(&b, "+++ %s%s\n", name, tab)
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(h.OldStart, h.OldLines), hunkRange(h.NewStart, h.NewLines))
		for _, l := range h.Lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// hunkRange is a git hunk range. A side with no lines names the line before it.
func hunkRange(start, count int) string {
	if count == 0 {
		return strconv.Itoa(max(0, start-1)) + ",0"
	}
	if count == 1 {
		return strconv.Itoa(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// quoteGitPath quotes a path as git does with the default core.quotePath.
func quoteGitPath(name string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			quoted = true
		case strings.IndexByte("\a\b\t\n\v\f\r", c) >= 0:
			b.WriteByte('\\')
			b.WriteByte("abtnvfr"[strings.IndexByte("\a\b\t\n\v\f\r", c)])
			quoted = true
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
			quoted = true
		default:
			b.WriteByte(c)
		}
	}
	if quoted {
		return `"` + b.String() + `"`
	}
	return b.String()
}

// --- generic results --------------------------------------------------------

// resultContent turns a tool_result's content into tool call content: each
// block as it is, an error as a fenced block so it renders as output.
func resultContent(raw json.RawMessage, isError bool) []acp2.ToolCallContent {
	if len(raw) == 0 {
		return nil
	}
	wrap := func(s string) string {
		if isError {
			return "```\n" + s + "\n```"
		}
		return s
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []acp2.ToolCallContent{acp2.ToolText(wrap(s))}
	}
	var blocks []resultBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []acp2.ToolCallContent
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, acp2.ToolText(wrap(b.Text)))
			}
		case "image":
			if b.Source.Type == "base64" {
				out = append(out, schema.NewToolCallContent(schema.ToolCallContentContent{
					Content: schema.NewContentBlock(schema.ContentBlockImage{Data: b.Source.Data, MIMEType: schema.MediaType(b.Source.MediaType)}),
				}))
			} else if b.Source.URL != "" {
				out = append(out, acp2.ToolText(wrap("[image: "+b.Source.URL+"]")))
			}
		case "document":
			out = append(out, acp2.ToolText(wrap("[document"+quotedTitle(b.Title)+"]")))
		case "tool_reference":
			out = append(out, acp2.ToolText(wrap("Tool: "+b.ToolName)))
		default:
			if j, err := json.Marshal(b); err == nil && b.Type != "" {
				out = append(out, acp2.ToolText(wrap(string(j))))
			}
		}
	}
	return out
}

func quotedTitle(t string) string {
	if t == "" {
		return ""
	}
	return ` "` + t + `"`
}

// resultBlock is one block of a tool_result's content array.
type resultBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Title    string `json:"title,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Source   struct {
		Type      string `json:"type,omitempty"`
		Data      string `json:"data,omitempty"`
		MediaType string `json:"media_type,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitzero"`
}

// toolResultUpdate builds the tool_call_update that finishes a tool use.
func (t *translator) toolResultUpdate(b block, structured json.RawMessage) schema.SessionUpdateToolCallUpdate {
	status := schema.ToolCallStatusCompleted
	if b.IsError {
		status = schema.ToolCallStatusFailed
	}
	upd := schema.SessionUpdateToolCallUpdate{
		ToolCallID: acp2.ToolCallID(b.ToolUseID),
		Status:     optional.Of(status),
	}
	info := t.tools[b.ToolUseID]
	if (info.name == "Edit" || info.name == "Write" || info.name == "MultiEdit") && !b.IsError {
		if content, locs, ok := fileDiff(structured); ok {
			upd.Content = optional.Of(content)
			upd.Locations = optional.Of(locs)
			return upd
		}
		// No diff to show: a confirmation text would only repeat the call.
		return upd
	}
	if content := resultContent(b.Content, b.IsError); len(content) > 0 {
		upd.Content = optional.Of(content)
	}
	return upd
}

// --- plans ------------------------------------------------------------------

const checklistPlanID = "plan"

// planTool reports whether a tool only updates the session's plan: such a tool
// call shows no tool call of its own.
func planTool(name string) bool {
	switch name {
	case "TodoWrite", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet":
		return true
	}
	return false
}

type taskEntry struct {
	subject    string
	status     string
	activeForm string
}

// taskList is the session's task list, as built from TaskCreate / TaskUpdate /
// TaskList calls, in creation order.
type taskList struct {
	order   []string
	entries map[string]*taskEntry
}

func (l *taskList) set(id string, e taskEntry) {
	if l.entries == nil {
		l.entries = map[string]*taskEntry{}
	}
	if _, ok := l.entries[id]; !ok {
		l.order = append(l.order, id)
	}
	l.entries[id] = &e
}

func (l *taskList) remove(id string) {
	if _, ok := l.entries[id]; !ok {
		return
	}
	delete(l.entries, id)
	for i, o := range l.order {
		if o == id {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
}

func (l *taskList) planEntries() []schema.PlanEntry {
	var out []schema.PlanEntry
	for _, id := range l.order {
		e := l.entries[id]
		out = append(out, planEntry(e.subject, e.activeForm, e.status))
	}
	return out
}

// planEntry shows the active form of the task in progress, as the reference does.
func planEntry(content, activeForm, status string) schema.PlanEntry {
	if status == "in_progress" && activeForm != "" {
		content = activeForm
	}
	st := schema.PlanEntryStatusPending
	switch status {
	case "in_progress":
		st = schema.PlanEntryStatusInProgress
	case "completed":
		st = schema.PlanEntryStatusCompleted
	}
	return schema.PlanEntry{Content: content, Priority: schema.PlanEntryPriorityMedium, Status: st}
}

// planUpdate replaces the checklist: plan_update, or plan_removed when empty.
func planUpdate(entries []schema.PlanEntry) backend.Event {
	if len(entries) == 0 {
		return update(schema.SessionUpdatePlanRemoved{PlanID: checklistPlanID})
	}
	return update(schema.SessionUpdatePlanUpdate{
		Plan: schema.NewPlanUpdateContent(schema.PlanUpdateContentItems{PlanID: checklistPlanID, Entries: entries}),
	})
}

// todoWritePlan is a TodoWrite call's checklist.
func todoWritePlan(input json.RawMessage) []schema.PlanEntry {
	var in struct {
		Todos []struct {
			Content    string `json:"content"`
			Status     string `json:"status"`
			ActiveForm string `json:"activeForm"`
		} `json:"todos"`
	}
	_ = json.Unmarshal(input, &in)
	var out []schema.PlanEntry
	for _, td := range in.Todos {
		out = append(out, planEntry(td.Content, td.ActiveForm, td.Status))
	}
	return out
}

var (
	taskCreatedRe = regexp.MustCompile(`^Task #(\S+) created successfully: (.+)$`)
	taskLineRe    = regexp.MustCompile(`^#(\S+) \[(pending|in_progress|completed)\] (.+)$`)
)

// applyTaskResult updates the task list from a finished Task* tool call and
// reports whether the list changed. input is the call's input; text the
// result's text. The live stream and the replay of a transcript both read the
// list through it, so a resumed session shows the plan it ended with.
func applyTaskResult(l *taskList, name string, input json.RawMessage, structured json.RawMessage, text string) bool {
	switch name {
	case "TaskCreate":
		var in struct{ Subject, ActiveForm string }
		var raw map[string]any
		_ = json.Unmarshal(input, &raw)
		in.Subject, _ = raw["subject"].(string)
		in.ActiveForm, _ = raw["activeForm"].(string)
		var out struct {
			Task struct {
				ID      string `json:"id"`
				Subject string `json:"subject"`
			} `json:"task"`
		}
		id, subject := "", in.Subject
		if json.Unmarshal(structured, &out) == nil && out.Task.ID != "" {
			id = out.Task.ID
			if out.Task.Subject != "" {
				subject = out.Task.Subject
			}
		} else if m := taskCreatedRe.FindStringSubmatch(strings.TrimSpace(text)); m != nil {
			id, subject = m[1], m[2]
		}
		if id == "" {
			return false
		}
		l.set(id, taskEntry{subject: subject, status: "pending", activeForm: in.ActiveForm})
		return true
	case "TaskUpdate":
		var in struct {
			TaskID     string `json:"taskId"`
			Status     string `json:"status"`
			Subject    string `json:"subject"`
			ActiveForm string `json:"activeForm"`
		}
		if json.Unmarshal(input, &in) != nil || in.TaskID == "" {
			return false
		}
		if in.Status == "deleted" {
			l.remove(in.TaskID)
			return true
		}
		e := taskEntry{}
		if old := l.entries[in.TaskID]; old != nil {
			e = *old
		}
		if in.Subject != "" {
			e.subject = in.Subject
		}
		if in.ActiveForm != "" {
			e.activeForm = in.ActiveForm
		}
		if in.Status != "" {
			e.status = in.Status
		}
		if e.subject == "" {
			return false // no content to show
		}
		if e.status == "" {
			e.status = "pending"
		}
		l.set(in.TaskID, e)
		return true
	case "TaskList":
		text = strings.TrimSpace(text)
		prev := l.entries
		var fresh taskList
		if text != "No tasks found" {
			for _, ln := range strings.Split(text, "\n") {
				m := taskLineRe.FindStringSubmatch(ln)
				if m == nil {
					return false // not the listing we know
				}
				e := taskEntry{subject: m[3], status: m[2]}
				if old := prev[m[1]]; old != nil {
					e.activeForm = old.activeForm
				}
				fresh.set(m[1], e)
			}
		}
		*l = fresh
		return true
	}
	return false
}

// resultPlainText is a tool_result's text, however the content is shaped.
func resultPlainText(raw json.RawMessage) string { return resultText(raw) }
