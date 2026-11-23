package codex

import (
	"context"
	"slices"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type model struct {
	ID          string            `json:"model"`
	DisplayName string            `json:"displayName"`
	Default     bool              `json:"isDefault"`
	Effort      string            `json:"defaultReasoningEffort"`
	Efforts     []reasoningEffort `json:"supportedReasoningEfforts"`
}

type reasoningEffort struct {
	ID string `json:"reasoningEffort"`
}

func (p *process) models(ctx context.Context, provider string) []model {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	params := object{"limit": 100}
	if provider != "" {
		params["modelProvider"] = provider
	}
	var response struct {
		Data []model `json:"data"`
	}
	_ = p.call(ctx, "model/list", params, &response)
	return response.Data
}

func (b *Backend) Options(ctx context.Context, cwd string, dirs []string) (backend.Options, error) {
	if _, err := b.workspace(cwd, dirs); err != nil {
		return backend.Options{}, err
	}
	p, err := b.connect(ctx)
	if err != nil {
		return backend.Options{}, err
	}
	catalog, err := b.setupCatalog(ctx, p, cwd, nil)
	if err != nil {
		return backend.Options{}, err
	}
	out := options(catalog.Model, catalog.Effort, catalog.Models)
	out.Config = appendMode(out.Config, catalog.Profile, catalog.Profiles)
	out.Config = appendCollaboration(out.Config, "", catalog.Model)
	if catalog.Provider != "" {
		choices := make([]schema.SessionConfigSelectOption, 0, len(catalog.Providers))
		for _, provider := range catalog.Providers {
			choices = append(choices, choice(provider.ID, provider.Name))
		}
		if !slices.ContainsFunc(choices, func(o schema.SessionConfigSelectOption) bool { return string(o.Value) == catalog.Provider }) {
			choices = append(choices, choice(catalog.Provider, catalog.Provider))
		}
		selectProvider := providerOption(catalog.Provider, "Provider for new sessions", choices...)
		out.Config = append([]acp2.SessionConfigOption{selectProvider}, out.Config...)
	}
	return out, nil
}

func (s *Session) Options() backend.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.optionsLocked()
}

func (s *Session) optionsLocked() backend.Options {
	out := options(s.model, s.effort, s.models)
	out.Config = appendMode(out.Config, s.profile, s.profiles)
	out.Config = appendCollaboration(out.Config, s.mode, s.model)
	if s.provider != "" {
		out.Config = append(out.Config, providerOption(s.provider, "Provider selected when this session was created", choice(s.provider, s.provider)))
	}
	return out
}

func providerOption(current, description string, choices ...schema.SessionConfigSelectOption) acp2.SessionConfigOption {
	cat := schema.SessionConfigOptionCategoryModelConfig
	return schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
		ConfigID: "provider", Name: "API provider", Category: &cat,
		Description: &description, CurrentValue: schema.SessionConfigValueID(current),
		Options: acp2.SelectOptions(choices...),
	})
}

func collaborationMode(value string) bool {
	return value == "plan" || value == "default"
}

func appendCollaboration(config []acp2.SessionConfigOption, current, model string) []acp2.SessionConfigOption {
	if model == "" {
		return config
	}
	if current == "" {
		current = "default"
	}
	return append(config, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
		ConfigID: "collaboration_mode", Name: "Collaboration mode",
		CurrentValue: schema.SessionConfigValueID(current),
		Options:      acp2.SelectOptions(choice("default", "Default"), choice("plan", "Plan")),
	}))
}

func appendMode(config []acp2.SessionConfigOption, current string, profiles []profile) []acp2.SessionConfigOption {
	var choices []schema.SessionConfigSelectOption
	for _, p := range profiles {
		if p.Allowed && p.ID != "" {
			label := p.ID
			switch p.ID {
			case ":read-only":
				label = "Read only"
			case ":workspace":
				label = "Workspace write"
			case ":danger-full-access":
				label = "Full access"
			}
			entry := choice(p.ID, label)
			entry.Description = p.Description
			choices = append(choices, entry)
		}
	}
	if len(choices) == 0 {
		return config
	}
	if current == "" {
		current = "default"
	}
	if !slices.ContainsFunc(choices, func(o schema.SessionConfigSelectOption) bool { return string(o.Value) == current }) {
		label := current
		if current == "default" {
			label = "Server default"
		}
		choices = append(choices, choice(current, label))
	}
	cat := schema.SessionConfigOptionCategoryMode
	return append(config, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
		ConfigID: "mode", Name: "Permissions", Category: &cat,
		CurrentValue: schema.SessionConfigValueID(current), Options: acp2.SelectOptions(choices...),
	}))
}

func (s *Session) SetConfigOption(ctx context.Context, id string, value any) ([]acp2.SessionConfigOption, error) {
	v, ok := value.(string)
	if !ok {
		return nil, backend.InvalidParams("%s requires a string value", id)
	}
	if err := s.configure(ctx, []backend.ConfigSelection{{ID: id, Value: v}}, false); err != nil {
		return nil, err
	}
	return s.Options().Config, nil
}

func (s *Session) ConfigureInitial(ctx context.Context, values []backend.ConfigSelection) error {
	return s.configure(ctx, values, true)
}

func (s *Session) configure(ctx context.Context, values []backend.ConfigSelection, initial bool) error {
	s.ops.Lock()
	defer s.ops.Unlock()
	s.mu.Lock()
	state := selectionOptions{Model: s.model, Effort: s.effort, Models: s.models, Profile: s.profile, Profiles: s.profiles}
	oldProfile := s.profile
	mode := s.mode
	sendMode := s.sendMode
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return &backend.SessionNotFoundError{ID: s.id}
	}
	for _, item := range values {
		if item.ID == "collaboration_mode" && state.Model != "" && collaborationMode(item.Value) {
			mode, sendMode = item.Value, true
			continue
		}
		if item.ID == "provider" && s.provider != "" && s.provider == item.Value {
			continue
		}
		// Provider is selected when opening a thread. It is not a next-turn
		// setting on an already loaded thread.
		valid := !(item.ID == "mode" && item.Value == "default" && oldProfile != "") &&
			state.selectValue(item.ID, item.Value)
		if !valid && !initial {
			return backend.InvalidParams("unknown or unavailable Codex option %s=%q", item.ID, item.Value)
		}
	}
	if state.Profile != oldProfile {
		if err := s.p.call(ctx, "thread/settings/update", object{"threadId": s.id, "permissions": state.Profile}, nil); err != nil {
			return threadError(s.id, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &backend.SessionNotFoundError{ID: s.id}
	}
	s.model, s.effort, s.profile, s.mode, s.sendMode = state.Model, state.Effort, state.Profile, mode, sendMode
	s.emit(update(schema.SessionUpdateConfigOptionUpdate{ConfigOptions: s.optionsLocked().Config}))
	return nil
}

func options(current, effort string, models []model) backend.Options {
	if current == "" {
		return backend.Options{}
	}
	var configs []acp2.SessionConfigOption
	var choices, efforts []schema.SessionConfigSelectOption
	seen := make(map[string]bool)
	for _, m := range models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		choices = append(choices, choice(m.ID, m.DisplayName))
		if m.ID == current {
			for _, e := range m.Efforts {
				efforts = append(efforts, choice(e.ID, e.ID))
			}
			if effort == "" {
				effort = m.Effort
			}
		}
	}
	if !seen[current] {
		choices = append(choices, choice(current, current))
	}
	modelCategory := schema.SessionConfigOptionCategoryModel
	configs = append(configs, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
		ConfigID: "model", Name: "Model", Category: &modelCategory,
		CurrentValue: schema.SessionConfigValueID(current), Options: acp2.SelectOptions(choices...),
	}))
	if slices.ContainsFunc(efforts, func(e schema.SessionConfigSelectOption) bool { return string(e.Value) == effort }) {
		effortCategory := schema.SessionConfigOptionCategoryThoughtLevel
		configs = append(configs, schema.NewSessionConfigOption(schema.SessionConfigOptionSelect{
			ConfigID: "reasoning_effort", Name: "Reasoning effort", Category: &effortCategory,
			CurrentValue: schema.SessionConfigValueID(effort), Options: acp2.SelectOptions(efforts...),
		}))
	}
	return backend.Options{Config: configs}
}

func choice(id, label string) schema.SessionConfigSelectOption {
	if label == "" {
		label = id
	}
	return schema.SessionConfigSelectOption{Value: schema.SessionConfigValueID(id), Name: label}
}
