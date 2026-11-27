package claude

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// system translates the system messages that report: an API retry, a
// compaction. A compaction follows claude-agent-acp's context-compaction.ts:
// compaction_update in_progress when Claude Code starts it, completed or failed
// when it ends. Claude Code reports the end twice (a status, then a
// compact_boundary): the second is only the context left.
func (t *translator) system(l *line) []backend.Event {
	switch l.Subtype {
	case "api_retry":
		// Claude Code is retrying a failed API request. Without a notice the
		// turn looks stuck: a sign-out retries for minutes.
		return one(retryNotice(l))
	case "task_started", "task_updated", "task_notification", "task_progress", "background_tasks_changed":
		return t.task(l)
	case "status":
		switch {
		case l.Status != nil && *l.Status == "compacting":
			return t.compactionStart(l.UUID)
		case l.CompactResult == "success":
			return t.compactionEnd(schema.CompactionStatusCompleted, "")
		case l.CompactResult == "failed":
			return t.compactionEnd(schema.CompactionStatusFailed, l.CompactError)
		}
	case "compact_boundary":
		// A compaction whose status was missed still ends here.
		out := t.compactionEnd(schema.CompactionStatusCompleted, "")
		if len(out) == 0 && !t.compacted {
			out = t.compactionOnce(l.UUID)
		}
		t.compacted = false
		return append(out, t.usage.compacted(l.CompactMeta.PostTokens))
	}
	return nil
}

func compactionUpdate(id string, status schema.CompactionStatus, errText string) backend.Event {
	v := schema.SessionUpdateCompactionUpdate{CompactionID: schema.CompactionID(id), Status: status}
	if errText != "" {
		v.Error = optional.Of(errText)
	}
	return update(v)
}

func (t *translator) compactionStart(id string) []backend.Event {
	if id == "" {
		id = newUUID()
	}
	t.compaction = id
	t.compacted = false
	t.streamedSummary = ""
	t.duplicateCompactErr = ""
	return one(compactionUpdate(id, schema.CompactionStatusInProgress, ""))
}

// compactionEnd ends the compaction in progress. It returns nothing when none is.
func (t *translator) compactionEnd(status schema.CompactionStatus, errText string) []backend.Event {
	if t.compaction == "" {
		return nil
	}
	id := t.compaction
	t.compaction = ""
	if status == schema.CompactionStatusCompleted {
		t.lastCompaction = id
		t.compacted = true
	} else {
		t.lastCompaction = ""
		t.compacted = false
	}
	if status == schema.CompactionStatusFailed && strings.TrimSpace(errText) != "" {
		t.duplicateCompactErr = strings.TrimSpace(errText)
	}
	return one(compactionUpdate(id, status, errText))
}

// consumeDuplicateCompactError suppresses the duplicate local_command_output
// Claude Code emits after a failed manual /compact (mirrors context-compaction.ts).
func (t *translator) consumeDuplicateCompactError(content string) bool {
	if t.duplicateCompactErr == "" || strings.TrimSpace(content) != t.duplicateCompactErr {
		return false
	}
	t.duplicateCompactErr = ""
	return true
}

// compactionOnce reports a compaction that ended without ever being seen starting.
func (t *translator) compactionOnce(id string) []backend.Event {
	if id == "" {
		id = newUUID()
	}
	t.lastCompaction = id
	return one(compactionUpdate(id, schema.CompactionStatusCompleted, ""))
}

// compactionChunk streams a piece of the summary that the API's compaction
// block carries. Only into a compaction Claude Code announced: the block holds
// no end of its own, so it never opens one (the reference does the same).
func (t *translator) compactionChunk(text string) []backend.Event {
	if t.compaction == "" || text == "" {
		return nil
	}
	t.streamedSummary += text
	return one(update(schema.SessionUpdateCompactionSummaryChunk{
		CompactionID: schema.CompactionID(t.compaction),
		Content:      acp2.TextBlock(text),
	}))
}

// compactionCancel ends a compaction that is still open when its turn ends,
// and resets turn-scoped compaction state so it cannot bleed into later turns.
func (t *translator) compactionCancel() []backend.Event {
	evs := t.compactionEnd(schema.CompactionStatusCancelled, "")
	t.lastCompaction = ""
	t.compacted = false
	t.streamedSummary = ""
	t.duplicateCompactErr = ""
	return evs
}

// compactSummary reports the summary that Claude Code keeps after a compaction,
// which it sends as a synthetic user message after the compaction ended.
func (t *translator) compactSummary(l *line) []backend.Event {
	if (!l.IsSynthetic && !l.IsCompactSummary) || t.lastCompaction == "" {
		return nil
	}
	var m message
	var text string
	if json.Unmarshal(l.Message, &m) != nil || json.Unmarshal(m.Content, &text) != nil {
		return nil
	}
	var summary string
	if l.IsCompactSummary {
		summary = compactionSummaryText(text)
	} else {
		summary = persistedContinuationSummary(text)
	}
	if summary == "" {
		return nil
	}
	id := t.lastCompaction
	t.lastCompaction = ""
	// A summary that went out as chunks is not sent again in full, unless the
	// cleaned text differs from what streamed.
	streamed := t.streamedSummary
	t.streamedSummary = ""
	if streamed == summary {
		return nil
	}
	return one(update(schema.SessionUpdateCompactionUpdate{
		CompactionID: schema.CompactionID(id),
		Status:       schema.CompactionStatusCompleted,
		Summary:      optional.Of([]acp2.ContentBlock{acp2.TextBlock(summary)}),
	}))
}

var (
	analysisBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?im)^[ \t]*<analysis>[ \t]*\r?\n[\s\S]*?\n[ \t]*</analysis>[ \t]*$`),
		regexp.MustCompile(`(?i)^\s*<analysis>[\s\S]*?</analysis>`),
	}
	summaryOpenLine      = regexp.MustCompile(`(?im)^[ \t]*<summary>[ \t]*\r?\n`)
	summaryCloseLine     = regexp.MustCompile(`(?im)^[ \t]*</summary>[ \t]*$`)
	continuationPreamble = regexp.MustCompile(`(?i)^\s*This session is being continued from a previous conversation`)
	persistedPreamble    = regexp.MustCompile(`^\s*This session is being continued from a previous conversation[^\n]*\n(?:[\s\S]*?\n)?Summary:[ \t]*\r?\n`)
	persistedTrailers    = []*regexp.Regexp{
		regexp.MustCompile(`\n+If you need specific details from before compaction[\s\S]*$`),
		regexp.MustCompile(`\n+(?:Please )?[Cc]ontinue the conversation from where it left (?:it )?off[\s\S]*$`),
	}
)

// persistedContinuationSummary extracts a summary only when the text carries
// Claude Code's continuation preamble (so unrelated synthetic messages that
// happen to contain HTML `<summary>` tags are never misread as compaction text).
func persistedContinuationSummary(raw string) string {
	text := raw
	for _, block := range analysisBlocks {
		text = block.ReplaceAllString(text, "")
	}
	if !continuationPreamble.MatchString(text) {
		return ""
	}
	return stripContinuationFraming(text)
}

func stripContinuationFraming(text string) string {
	body := persistedPreamble.ReplaceAllString(text, "")
	if body == text {
		return ""
	}
	for _, tr := range persistedTrailers {
		body = tr.ReplaceAllString(body, "")
	}
	return strings.TrimSpace(body)
}

// compactionSummaryText reduces the continuation message Claude Code persists to
// the summary a user can read, without the instructions or `<analysis>`/`<summary>`
// tags that frame it for the model. A message whose framing is not recognised
// yields no summary.
func compactionSummaryText(raw string) string {
	text := raw
	for _, block := range analysisBlocks {
		text = block.ReplaceAllString(text, "")
	}
	if tagged, ok := taggedSummary(text); ok {
		return strings.TrimSpace(tagged)
	}
	if !continuationPreamble.MatchString(text) {
		return ""
	}
	return stripContinuationFraming(text)
}

func taggedSummary(raw string) (string, bool) {
	closes := summaryCloseLine.FindAllStringIndex(raw, -1)
	if len(closes) > 0 {
		closeStart := closes[len(closes)-1][0]
		opens := summaryOpenLine.FindAllStringIndex(raw[:closeStart], -1)
		if len(opens) > 0 {
			openEnd := opens[len(opens)-1][1]
			return raw[openEnd:closeStart], true
		}
	}
	lower := strings.ToLower(raw)
	openIdx := strings.Index(lower, "<summary>")
	closeIdx := strings.LastIndex(lower, "</summary>")
	if openIdx >= 0 && closeIdx > openIdx {
		return raw[openIdx+len("<summary>") : closeIdx], true
	}
	return "", false
}
