package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// Config option ids, as claude-agent-acp names them.
const (
	configMode   = "mode"
	configModel  = "model"
	configEffort = "effort"
)

// initInfo is the part of Claude Code's `initialize` answer that sessions use.
type initInfo struct {
	Commands []struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		ArgumentHint string `json:"argumentHint"`
	} `json:"commands"`
	Models                []modelInfo `json:"models"`
	CurrentPermissionMode string      `json:"current_permission_mode"`
}

type modelInfo struct {
	Value                string   `json:"value"`
	ResolvedModel        string   `json:"resolvedModel"`
	DisplayName          string   `json:"displayName"`
	Description          string   `json:"description"`
	SupportsEffort       bool     `json:"supportsEffort"`
	SupportsAutoMode     bool     `json:"supportsAutoMode"`
	SupportedEffortLevel []string `json:"supportedEffortLevels"`
}

// sessionConfig is what the user chose; "" is Claude Code's own default. It is
// kept in the session, and passed as flags when the process starts again.
type sessionConfig struct {
	mode, model, effort string
}

// initialize asks a fresh process what it offers. It is the session's first
// control request, and runs once: the answer does not change over the session.
func (s *Session) initialize(ctx context.Context, p *proc) error {
	body, err := s.call(ctx, p, map[string]any{"subtype": "initialize"})
	if err != nil {
		return fmt.Errorf("claude initialize: %w", err)
	}
	var info initInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return fmt.Errorf("claude initialize: %w", err)
	}
	s.mu.Lock()
	s.info = &info
	if s.opts.Mode == "" && info.CurrentPermissionMode != "" && info.CurrentPermissionMode != "default" {
		s.opts.Mode = info.CurrentPermissionMode
	}
	s.mu.Unlock()
	return nil
}

// Init starts the session's process and learns what it offers, so that the
// session can report its options right away. A session never prompted keeps
// --session-id for its first prompt: opts.New flips at the first Send.
func (s *Session) Init(ctx context.Context) error {
	p, err := s.ensureProc()
	if err != nil {
		return err
	}
	s.mu.Lock()
	done := s.info != nil
	s.mu.Unlock()
	if done {
		return nil
	}
	return s.initialize(ctx, p)
}

// Options returns the session's current config options and commands.
func (s *Session) Options() backend.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.optionsLocked()
}

func (s *Session) optionsLocked() backend.Options {
	if s.info == nil {
		return backend.Options{}
	}
	return backend.Options{
		Config:   buildConfigOptions(s.info, s.cfgLocked()),
		Commands: buildCommands(s.info),
	}
}

func (s *Session) cfgLocked() sessionConfig {
	return sessionConfig{mode: s.opts.Mode, model: s.opts.Model, effort: s.opts.Effort}
}

func buildCommands(info *initInfo) []acp2.AvailableCommand {
	out := make([]acp2.AvailableCommand, 0, len(info.Commands))
	for _, c := range info.Commands {
		name := strings.TrimPrefix(c.Name, "/")
		if name == "" {
			continue
		}
		cmd := schema.AvailableCommandCommand{Name: name, Description: c.Description}
		if c.ArgumentHint != "" {
			cmd.Input = schema.NewAvailableCommandInput(schema.AvailableCommandInputText{Hint: c.ArgumentHint})
		}
		out = append(out, schema.NewAvailableCommand(cmd))
	}
	return out
}

type modeInfo struct{ id, name, desc string }

// bypassAllowed reports whether the session can run without permission checks:
// Claude Code refuses that as root outside a sandbox.
var bypassAllowed = os.Geteuid() != 0 || os.Getenv("IS_SANDBOX") != ""

// autoFallback is the mode a session in Auto moves to when its model cannot
// serve Auto.
const autoFallback = "acceptEdits"

// availableModes is the mode catalog: Auto only while the model can serve it,
// bypass only where Claude Code allows it. Mirrors claude-agent-acp's
// buildAvailableModes.
func availableModes(info *initInfo, cfg sessionConfig) []modeInfo {
	out := []modeInfo{
		{"default", "Manual", "Always ask before making changes"},
		{"acceptEdits", "Accept edits", "Automatically accept all file edits"},
		{"plan", "Plan", "Create a plan before making changes"},
	}
	if m := currentModel(info, cfg); m == nil || m.SupportsAutoMode {
		out = append(out, modeInfo{"auto", "Auto", "Claude handles permission decisions"})
	}
	if bypassAllowed {
		out = append(out, modeInfo{"bypassPermissions", "Bypass permissions", "Accepts all permissions"})
	}
	return out
}

func findModel(models []modelInfo, value string) *modelInfo {
	for i := range models {
		if models[i].Value == value {
			return &models[i]
		}
	}
	return nil
}

func currentModel(info *initInfo, cfg sessionConfig) *modelInfo {
	value := firstNonEmpty(cfg.model, "default")
	return findModel(info.Models, value)
}

// buildConfigOptions lists the mode, model and effort options. The effort
// option exists only while the current model has effort levels.
func buildConfigOptions(info *initInfo, cfg sessionConfig) []acp2.SessionConfigOption {
	mode := schema.SessionConfigOptionCategoryMode
	var out []acp2.SessionConfigOption

	var modeOpts []schema.SessionConfigSelectOption
	for _, m := range availableModes(info, cfg) {
		d := m.desc
		modeOpts = append(modeOpts, schema.SessionConfigSelectOption{Value: schema.SessionConfigValueID(m.id), Name: m.name, Description: &d})
	}
	modeDesc := "Session permission mode"
	out = append(out, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
		ConfigID:     configMode,
		Name:         "Mode",
		Description:  &modeDesc,
		Category:     &mode,
		CurrentValue: schema.SessionConfigValueID(firstNonEmpty(cfg.mode, "default")),
		Options:      acp2.SelectOptions(modeOpts...),
	}))

	if len(info.Models) > 0 {
		cat := schema.SessionConfigOptionCategoryModel
		var opts []schema.SessionConfigSelectOption
		for _, m := range info.Models {
			o := schema.SessionConfigSelectOption{Value: schema.SessionConfigValueID(m.Value), Name: firstNonEmpty(m.DisplayName, m.Value)}
			if m.Description != "" {
				d := m.Description
				o.Description = &d
			}
			opts = append(opts, o)
		}
		desc := "Model for this session"
		out = append(out, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
			ConfigID:     configModel,
			Name:         "Model",
			Description:  &desc,
			Category:     &cat,
			CurrentValue: schema.SessionConfigValueID(firstNonEmpty(cfg.model, "default")),
			Options:      acp2.SelectOptions(opts...),
		}))
	}

	if m := currentModel(info, cfg); m != nil && m.SupportsEffort && len(m.SupportedEffortLevel) > 0 {
		cat := schema.SessionConfigOptionCategoryThoughtLevel
		opts := []schema.SessionConfigSelectOption{{Value: "default", Name: "Default"}}
		for _, l := range m.SupportedEffortLevel { // Claude Code's order: low to max
			opts = append(opts, schema.SessionConfigSelectOption{Value: schema.SessionConfigValueID(l), Name: effortName(l)})
		}
		desc := "Available effort levels for this model"
		cur := cfg.effort
		if cur == "" || !slices.Contains(m.SupportedEffortLevel, cur) {
			cur = "default"
		}
		out = append(out, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
			ConfigID:     configEffort,
			Name:         "Effort",
			Description:  &desc,
			Category:     &cat,
			CurrentValue: schema.SessionConfigValueID(cur),
			Options:      acp2.SelectOptions(opts...),
		}))
	}
	return out
}

func effortName(l string) string {
	if l == "xhigh" {
		return "Extra high"
	}
	if l == "" {
		return l
	}
	return strings.ToUpper(l[:1]) + l[1:]
}

// SetConfigOption applies one option. The new value is also reported as a
// config_option_update, so that every client watching the session sees it.
func (s *Session) SetConfigOption(ctx context.Context, id string, value any) ([]acp2.SessionConfigOption, error) {
	str, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("invalid params: config option %q takes a string value, got %v", id, value)
	}
	s.mu.Lock()
	info := s.info
	cfg := s.cfgLocked()
	s.mu.Unlock()
	if info == nil {
		return nil, fmt.Errorf("session options are not known yet")
	}

	var body map[string]any
	modeChanged := false // a model change moved the mode too
	switch id {
	case configMode:
		if !slices.ContainsFunc(availableModes(info, cfg), func(m modeInfo) bool { return m.id == str }) {
			return nil, fmt.Errorf("invalid params: invalid value for config option %s: %s", id, str)
		}
		body = map[string]any{"subtype": "set_permission_mode", "mode": str}
		cfg.mode = str
	case configModel:
		if findModel(info.Models, str) == nil {
			return nil, fmt.Errorf("invalid params: invalid value for config option %s: %s", id, str)
		}
		body = map[string]any{"subtype": "set_model", "model": str}
		cfg.model = str
		// The new model may not offer the effort level that was chosen.
		if m := findModel(info.Models, str); !m.SupportsEffort || !slices.Contains(m.SupportedEffortLevel, cfg.effort) {
			cfg.effort = ""
		}
		// A model that cannot serve Auto moves a session in Auto to the fallback.
		if cfg.mode == "auto" && !findModel(info.Models, str).SupportsAutoMode {
			cfg.mode = autoFallback
			modeChanged = true
		}
	case configEffort:
		m := currentModel(info, cfg)
		if str != "default" && (m == nil || !slices.Contains(m.SupportedEffortLevel, str)) {
			return nil, fmt.Errorf("invalid params: invalid value for config option %s: %s", id, str)
		}
		if str == "default" {
			str = ""
		}
		var level any = str
		if str == "" {
			level = nil
		}
		body = map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": level}}
		cfg.effort = str
	default:
		return nil, fmt.Errorf("invalid params: unknown config option %q", id)
	}

	// A live process learns it now; without one the value rides the next start.
	if p := s.rt.Get(s.uuid); p != nil {
		if _, err := s.call(ctx, p, body); err != nil {
			return nil, err
		}
		// A model change resets the effort the process holds when the old level is gone.
		if modeChanged {
			_, _ = s.call(ctx, p, map[string]any{"subtype": "set_permission_mode", "mode": cfg.mode})
		}
		if id == configModel && cfg.effort == "" {
			_, _ = s.call(ctx, p, map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": nil}})
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Mode, s.opts.Model, s.opts.Effort = cfg.mode, cfg.model, cfg.effort
	opts := s.optionsLocked()
	s.send(update(schema.SessionUpdateConfigOptionUpdate{ConfigOptions: opts.Config}))
	return opts.Config, nil
}

// --- titles -----------------------------------------------------------------

// maxTitleContext bounds the text a title is generated from, as Claude Code
// itself caps its title input.
const maxTitleContext = 1000

// askTitle has Claude Code title the session from its first prompt. Claude Code
// stores the title in the transcript a few seconds in and sends no message
// when it does, so the title is asked for with generate_session_title, which
// answers with the stored one or waits for the one being made. Later prompts
// ask again only while no title came.
func (s *Session) askTitle(p *proc, blocks []acp2.ContentBlock) {
	s.mu.Lock()
	if s.titled || s.titling {
		s.mu.Unlock()
		return
	}
	s.titling = true
	s.mu.Unlock()

	text := acp2.JoinTexts(blocks)
	if r := []rune(text); len(r) > maxTitleContext {
		text = string(r[len(r)-maxTitleContext:])
	}
	if strings.TrimSpace(text) == "" {
		s.mu.Lock()
		s.titling = false
		s.mu.Unlock()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		body, err := s.call(ctx, p, map[string]any{
			"subtype": "generate_session_title", "description": text, "persist": true,
		})
		var out struct {
			Title string `json:"title"`
		}
		if err == nil {
			_ = json.Unmarshal(body, &out)
		}
		title := sanitizeTitle(out.Title)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.titling = false
		if title == "" {
			return
		}
		s.titled = true
		s.send(update(schema.SessionUpdateSessionInfoUpdate{Title: optional.Of(title)}))
	}()
}
