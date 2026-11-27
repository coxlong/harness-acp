package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

const (
	skillsRefreshDelay    = 300 * time.Millisecond
	skillsRefreshMaxDelay = 2 * time.Second
)

type skillInfo struct {
	Name             string  `json:"name"`
	Description      *string `json:"description"`
	ShortDescription *string `json:"shortDescription"`
}

func (p *process) skills(ctx context.Context, cwds []string) ([]skillInfo, error) {
	if p == nil {
		return nil, nil
	}
	clean := make([]string, 0, len(cwds))
	for _, dir := range cwds {
		if dir != "" && !slices.Contains(clean, dir) {
			clean = append(clean, dir)
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var response struct {
		Data []struct {
			Skills []skillInfo `json:"skills"`
		} `json:"data"`
	}
	if err := p.call(ctx, "skills/list", object{"cwds": clean}, &response); err != nil {
		return nil, err
	}
	var out []skillInfo
	for _, entry := range response.Data {
		out = append(out, entry.Skills...)
	}
	return out, nil
}

func codexCommands(skills []skillInfo) []acp2.AvailableCommand {
	type spec struct {
		name string
		desc string
		hint string
	}
	builtins := []spec{
		{name: "plan", desc: "Turn plan mode on."},
		{name: "mcp", desc: "Show the status of the MCP servers."},
		{name: "skills", desc: "List available skills."},
		{name: "status", desc: "Display session configuration and token usage."},
		{name: "review", desc: "Review uncommitted changes, or review with custom instructions.", hint: "optional review instructions"},
		{name: "review-branch", desc: "Review changes relative to a base branch.", hint: "branch name"},
		{name: "review-commit", desc: "Review a specific commit.", hint: "commit sha"},
		{name: "compact", desc: "Summarize conversation to avoid hitting the context limit."},
		{name: "rename", desc: "Rename the current session.", hint: "new name"},
	}
	seen := make(map[string]bool, len(builtins)+len(skills))
	out := make([]acp2.AvailableCommand, 0, len(builtins)+len(skills))
	add := func(name, desc, hint string) {
		if name == "" || strings.IndexFunc(name, unicode.IsSpace) >= 0 || seen[name] {
			return
		}
		seen[name] = true
		cmd := schema.AvailableCommandCommand{Name: name, Description: desc}
		if hint != "" {
			cmd.Input = schema.NewAvailableCommandInput(schema.AvailableCommandInputText{Hint: hint})
		}
		out = append(out, schema.NewAvailableCommand(cmd))
	}
	for _, b := range builtins {
		add(b.name, b.desc, b.hint)
	}
	for _, sk := range skills {
		if sk.Name == "" {
			continue
		}
		desc := sk.Name
		if sk.ShortDescription != nil && *sk.ShortDescription != "" {
			desc = *sk.ShortDescription
		} else if sk.Description != nil && *sk.Description != "" {
			desc = *sk.Description
		}
		add("$"+sk.Name, desc, "")
	}
	return out
}

func commandsKey(cmds []acp2.AvailableCommand) string {
	raw, err := json.Marshal(cmds)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (s *Session) skillCwdsLocked() []string {
	cwds := make([]string, 0, 1+len(s.additionalDirs))
	if s.cwd != "" {
		cwds = append(cwds, s.cwd)
	}
	for _, dir := range s.additionalDirs {
		if dir != "" && !slices.Contains(cwds, dir) {
			cwds = append(cwds, dir)
		}
	}
	return cwds
}

func (s *Session) scheduleSkillsRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.parent != nil {
		return
	}
	if s.skillsTimer != nil {
		s.skillsTimer.Stop()
	}
	now := time.Now()
	if s.skillsDeadline.IsZero() {
		s.skillsDeadline = now.Add(skillsRefreshMaxDelay)
	}
	delay := min(skillsRefreshDelay, max(0, s.skillsDeadline.Sub(now)))
	s.skillsTimer = time.AfterFunc(delay, func() {
		s.refreshSkills()
	})
}

func (s *Session) refreshSkills() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.skillsDeadline = time.Time{}
	s.skillsRev++
	rev := s.skillsRev
	cwds := s.skillCwdsLocked()
	s.mu.Unlock()

	skills, err := s.p.skills(context.Background(), cwds)
	if err != nil {
		return
	}
	cmds := codexCommands(skills)
	key := commandsKey(cmds)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || rev != s.skillsRev {
		return
	}
	s.skills = skills
	if key != "" && key == s.publishedCmdsKey {
		return
	}
	s.publishedCmdsKey = key
	s.emit(update(schema.SessionUpdateAvailableCommandsUpdate{AvailableCommands: cmds}))
}

// --- slash command dispatch -------------------------------------------------

type parsedSlashCommand struct {
	name string
	rest string
}

func parseSlashCommand(blocks []acp2.ContentBlock) (parsedSlashCommand, bool) {
	if len(blocks) == 0 {
		return parsedSlashCommand{}, false
	}
	first, ok := blocks[0].Variant().(schema.ContentBlockText)
	if !ok {
		return parsedSlashCommand{}, false
	}
	text := strings.TrimSpace(first.Text)
	if !strings.HasPrefix(text, "/") {
		return parsedSlashCommand{}, false
	}
	body := strings.TrimSpace(text[1:])
	if body == "" {
		return parsedSlashCommand{}, false
	}
	end := strings.IndexFunc(body, unicode.IsSpace)
	var name, rest string
	if end < 0 {
		name = body
	} else {
		name = body[:end]
		rest = strings.TrimSpace(body[end:])
	}
	if name == "" || strings.HasPrefix(name, "$") {
		return parsedSlashCommand{}, false
	}
	return parsedSlashCommand{name: strings.ToLower(name), rest: rest}, true
}

// tryCommand intercepts built-in Codex slash commands (aligned with codex-acp's
// CodexCommands.ts). Callers hold s.ops.
func (s *Session) tryCommand(ctx context.Context, id acp2.MessageID, blocks []acp2.ContentBlock) (bool, error) {
	cmd, ok := parseSlashCommand(blocks)
	if !ok {
		return false, nil
	}
	switch cmd.name {
	case "plan":
		if cmd.rest != "" {
			return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
				return `Command "/plan" requires no arguments.`, nil
			})
		}
		return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
			s.mu.Lock()
			next := "plan"
			if s.mode == "plan" {
				next = "default"
			}
			s.mu.Unlock()
			return "", s.configureLocked(ctx, []backend.ConfigSelection{{ID: "collaboration_mode", Value: next}}, false)
		})
	case "status":
		return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
			return s.buildStatusMessage(), nil
		})
	case "rename":
		if cmd.rest == "" {
			return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
				return `Command "/rename" requires new name.`, nil
			})
		}
		return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
			if err := s.p.call(ctx, "thread/name/set", object{"threadId": s.id, "name": cmd.rest}, nil); err != nil {
				return "", threadError(s.id, err)
			}
			return "", nil
		})
	case "skills":
		return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
			s.mu.Lock()
			cwds := s.skillCwdsLocked()
			s.mu.Unlock()
			skills, err := s.p.skills(ctx, cwds)
			if err != nil {
				return "", err
			}
			if len(skills) == 0 {
				return "No skills configured.", nil
			}
			lines := make([]string, 0, len(skills)+1)
			lines = append(lines, "Available skills:")
			for _, sk := range skills {
				desc := ""
				if sk.ShortDescription != nil && *sk.ShortDescription != "" {
					desc = *sk.ShortDescription
				} else if sk.Description != nil && *sk.Description != "" {
					desc = *sk.Description
				}
				if desc != "" {
					lines = append(lines, fmt.Sprintf("- %s: %s", sk.Name, desc))
				} else {
					lines = append(lines, fmt.Sprintf("- %s", sk.Name))
				}
			}
			return strings.Join(lines, "\n"), nil
		})
	case "mcp":
		return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
			return s.runMcpStatus(ctx)
		})
	case "compact":
		return true, s.submitCommandTurn(ctx, id, blocks, "thread/compact/start", object{"threadId": s.id})
	case "review":
		target := object{"type": "uncommittedChanges"}
		if cmd.rest != "" {
			target = object{"type": "custom", "instructions": cmd.rest}
		}
		return true, s.submitCommandTurn(ctx, id, blocks, "review/start", object{
			"threadId": s.id,
			"target":   target,
			"delivery": "inline",
		})
	case "review-branch":
		if cmd.rest == "" {
			return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
				return `Command "/review-branch" requires branch name.`, nil
			})
		}
		return true, s.submitCommandTurn(ctx, id, blocks, "review/start", object{
			"threadId": s.id,
			"target":   object{"type": "baseBranch", "branch": cmd.rest},
			"delivery": "inline",
		})
	case "review-commit":
		if cmd.rest == "" {
			return true, s.runLocalCommand(cmd.name, id, blocks, func() (string, error) {
				return `Command "/review-commit" requires commit sha.`, nil
			})
		}
		return true, s.submitCommandTurn(ctx, id, blocks, "review/start", object{
			"threadId": s.id,
			"target":   object{"type": "commit", "sha": cmd.rest, "title": nil},
			"delivery": "inline",
		})
	default:
		return false, nil
	}
}

// runLocalCommand obeys ACP v2's local command lifecycle: emits the live user
// message, transitions idle -> running -> idle(end_turn) when no foreground
// turn is already open, and emits any reply text as an agent message.
func (s *Session) runLocalCommand(name string, id acp2.MessageID, blocks []acp2.ContentBlock, run func() (string, error)) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &backend.SessionNotFoundError{ID: s.id}
	}
	opened := s.turnID == "" && !s.running
	s.emit(update(schema.SessionUpdateUserMessage{MessageID: id, Content: optional.Of(blocks)}))
	if opened {
		s.running = true
		s.state(backend.TurnRunning, "", nil)
	}
	s.mu.Unlock()

	reply, err := run()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if reply == "" && err != nil && (!opened || s.turnID != "") {
		// Another turn owns the foreground lifecycle, so idle(error) will not
		// fire here; surface the failure directly as an agent message.
		reply = fmt.Sprintf("The '/%s' command failed: %v", name, err)
	}
	if reply != "" {
		msgID := acp2.MessageID(fmt.Sprintf("%s:reply", id))
		s.emit(update(schema.SessionUpdateAgentMessage{
			MessageID: msgID,
			Content:   optional.Of([]acp2.ContentBlock{acp2.TextBlock(reply)}),
		}))
	}
	if opened && s.turnID == "" {
		s.running = false
		if err != nil {
			s.state(backend.TurnIdle, "", err)
		} else {
			s.state(backend.TurnIdle, acp2.StopReasonEndTurn, nil)
		}
	}
	return nil
}

// submitCommandTurn invokes a Codex app-server RPC that starts a runtime turn
// (`thread/compact/start` or `review/start`), staging concurrent notifications
// so `user_message` and `running` always precede early turn/compaction frames.
func (s *Session) submitCommandTurn(ctx context.Context, id acp2.MessageID, blocks []acp2.ContentBlock, method string, params object) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return &backend.SessionNotFoundError{ID: s.id}
	}
	s.submitting = true
	s.mu.Unlock()

	var result struct {
		Turn turn `json:"turn"`
	}
	err := s.p.call(ctx, method, params, &result)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitting = false
	staged := s.staged
	s.staged = nil
	if s.closed {
		return &backend.SessionNotFoundError{ID: s.id}
	}
	if err == nil {
		s.emit(update(schema.SessionUpdateUserMessage{MessageID: id, Content: optional.Of(blocks)}))
		s.published[string(id)] = true
		if result.Turn.ID != "" {
			s.startTurn(result.Turn.ID)
			if method == "review/start" {
				s.reviewTurn = true
			}
		} else if !s.running {
			s.running = true
			s.state(backend.TurnRunning, "", nil)
		}
	}
	for _, m := range staged {
		s.handle(m)
	}
	return threadError(s.id, err)
}

func (s *Session) buildStatusMessage() string {
	s.mu.Lock()
	model := s.model
	effort := s.effort
	cwd := s.cwd
	profile := s.profile
	mode := s.mode
	id := s.id
	usage := s.lastUsage
	s.mu.Unlock()

	if model == "" {
		model = "default"
	}
	if mode == "" {
		mode = "default"
	}
	if profile == "" {
		profile = "default"
	}
	modelLine := model
	if effort != "" {
		modelLine = fmt.Sprintf("%s (%s)", model, effort)
	}
	lines := []string{
		fmt.Sprintf("**Model:** %s", modelLine),
		fmt.Sprintf("**Directory:** %s", cwd),
		fmt.Sprintf("**Permissions:** %s", profile),
		fmt.Sprintf("**Collaboration mode:** %s", mode),
		fmt.Sprintf("**Session:** `%s`", id),
	}
	if usage != nil {
		lines = append(lines, "")
		lines = append(lines, fmt.Sprintf("**Token usage:** %s total (%s input + %s cached input, %s output)",
			fmtTokenCount(usage.Total.TotalTokens),
			fmtTokenCount(usage.Total.InputTokens),
			fmtTokenCount(usage.Total.CachedInputTokens),
			fmtTokenCount(usage.Total.OutputTokens),
		))
		if usage.ModelContextWindow > 0 && usage.Last.TotalTokens > 0 {
			pct := (usage.Last.TotalTokens * 100) / usage.ModelContextWindow
			lines = append(lines, fmt.Sprintf("**Context window:** %d%% used (%s / %s)",
				pct,
				fmtTokenCount(usage.Last.TotalTokens),
				fmtTokenCount(usage.ModelContextWindow),
			))
		}
	}
	return strings.Join(lines, "  \n")
}

func (s *Session) runMcpStatus(ctx context.Context) (string, error) {
	var resp struct {
		Data []struct {
			Name       string         `json:"name"`
			AuthStatus string         `json:"authStatus"`
			Tools      map[string]any `json:"tools"`
		} `json:"data"`
	}
	if err := s.p.call(ctx, "mcpServerStatus/list", object{"limit": 100}, &resp); err != nil {
		return "", threadError(s.id, err)
	}
	if len(resp.Data) == 0 {
		return "No MCP servers configured.", nil
	}
	lines := make([]string, 0, len(resp.Data)+1)
	lines = append(lines, "MCP servers:")
	for _, srv := range resp.Data {
		status := srv.AuthStatus
		if status == "" {
			status = "connected"
		}
		lines = append(lines, fmt.Sprintf("- **%s**: %s (%d tools)", srv.Name, status, len(srv.Tools)))
	}
	return strings.Join(lines, "\n"), nil
}

func fmtTokenCount(n uint64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// --- compaction lifecycle (mirrors codex-acp/src/CodexSessionCompactions.ts) -

type codexCompaction struct {
	turnID     string
	id         string
	terminal   bool
	legacyOnly bool
}

func (s *Session) startCompaction(turnID, itemID string) {
	if itemID == "" {
		return
	}
	if _, exists := s.compactions[itemID]; exists {
		return
	}
	c := s.rememberCompaction(turnID, itemID, false)
	s.emit(update(schema.SessionUpdateCompactionUpdate{
		CompactionID: schema.CompactionID(c.id),
		Status:       schema.CompactionStatusInProgress,
	}))
}

func (s *Session) completeCompaction(turnID, itemID string) {
	if itemID == "" {
		return
	}
	if _, exists := s.compactions[itemID]; !exists {
		if latest := s.latestCompaction[turnID]; latest != nil && latest.legacyOnly {
			delete(s.compactions, latest.id)
			s.compactions[itemID] = latest
			latest.legacyOnly = false
		}
	}
	c := s.compactions[itemID]
	if c == nil {
		c = s.rememberCompaction(turnID, itemID, false)
	}
	s.finishCompaction(c, schema.CompactionStatusCompleted, "")
}

func (s *Session) completeLegacyCompaction(turnID string) {
	if turnID == "" {
		turnID = "unknown"
	}
	c := s.latestCompaction[turnID]
	if c == nil {
		c = s.rememberCompaction(turnID, "compaction:"+turnID, true)
	}
	s.finishCompaction(c, schema.CompactionStatusCompleted, "")
}

func (s *Session) finishTurnCompactions(turnID string, status schema.CompactionStatus, errMsg string) {
	for _, c := range s.compactions {
		if c.turnID == turnID || c.turnID == "" {
			s.finishCompaction(c, status, errMsg)
		}
	}
}

func (s *Session) finishAllCompactions(status schema.CompactionStatus) {
	for _, c := range s.compactions {
		s.finishCompaction(c, status, "")
	}
}

func (s *Session) rememberCompaction(turnID, id string, legacyOnly bool) *codexCompaction {
	c := &codexCompaction{turnID: turnID, id: id, legacyOnly: legacyOnly}
	s.compactions[id] = c
	if turnID != "" {
		s.latestCompaction[turnID] = c
	}
	return c
}

func (s *Session) finishCompaction(c *codexCompaction, status schema.CompactionStatus, errMsg string) {
	if c == nil || c.terminal {
		return
	}
	c.terminal = true
	u := schema.SessionUpdateCompactionUpdate{
		CompactionID: schema.CompactionID(c.id),
		Status:       status,
	}
	if status == schema.CompactionStatusFailed && errMsg != "" {
		u.Error = optional.Of(errMsg)
	}
	s.emit(update(u))
}
