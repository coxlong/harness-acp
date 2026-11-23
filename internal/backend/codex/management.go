package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"

	"github.com/coxlong/harness-acp/internal/backend"
)

func (b *Backend) ForkSession(ctx context.Context, id string, f backend.NewSessionParams) (backend.SessionHandle, error) {
	p, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	source, err := b.readThread(ctx, p, id, false)
	if err != nil {
		return nil, err
	}
	if source.ParentID != "" {
		return nil, &backend.SessionNotFoundError{ID: id}
	}
	params, err := b.workspace(f.Cwd, f.AdditionalDirectories)
	if err != nil {
		return nil, err
	}
	params["threadId"], params["excludeTurns"] = id, true
	params["runtimeWorkspaceRoots"] = append([]string{f.Cwd}, f.AdditionalDirectories...)
	mode, err := b.applySetupConfig(ctx, p, f.Cwd, &source, f.Config, params)
	if err != nil {
		return nil, err
	}
	return b.open(ctx, "thread/fork", params, mode)
}

func (b *Backend) ArchiveSession(ctx context.Context, id string, archived bool) error {
	method := "thread/unarchive"
	if archived {
		method = "thread/archive"
	}
	err := b.mutateThread(ctx, id, method, nil)
	if err == nil {
		return nil
	}
	p, connectionErr := b.connect(ctx)
	if connectionErr != nil {
		return err
	}
	// Native archive RPCs look for the source rollout. After a retry that
	// rollout has already moved, so confirm the target via the scoped index.
	rows, _, lookupErr := b.listNative(ctx, p, "", 1, archived, nil, id)
	if lookupErr == nil && slices.ContainsFunc(rows, func(row backend.SessionInfo) bool { return row.ID == id }) {
		return nil
	}
	return err
}

func (b *Backend) SetSessionTitle(ctx context.Context, id, title string) error {
	return b.mutateThread(ctx, id, "thread/name/set", object{"name": title})
}

func (b *Backend) mutateThread(ctx context.Context, id, method string, params object) error {
	p, err := b.connect(ctx)
	if err != nil {
		return err
	}
	t, err := b.readThread(ctx, p, id, false)
	if err != nil {
		return err
	}
	if t.ParentID != "" {
		return &backend.SessionNotFoundError{ID: id}
	}
	if params == nil {
		params = object{}
	}
	params["threadId"] = id
	return threadError(id, p.call(ctx, method, params, nil))
}

type listPosition struct {
	Cursor *string
	Skip   int
	Done   bool
}

type combinedCursor struct {
	Active   listPosition
	Archived listPosition
}

func (b *Backend) listCombined(ctx context.Context, p *process, f backend.ListFilter, cursor *string) ([]backend.SessionInfo, *string, error) {
	var state combinedCursor
	if cursor != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*cursor)
		if err != nil || json.Unmarshal(raw, &state) != nil ||
			state.Active.Skip < 0 || state.Archived.Skip < 0 {
			return nil, nil, backend.InvalidParams("invalid combined session cursor")
		}
	}
	const nativePageSize = 100
	type page struct {
		items []backend.SessionInfo
		next  *string
		pos   *listPosition
	}
	pull := func(pos *listPosition, archived bool) (page, error) {
		for !pos.Done {
			items, next, err := b.listNative(ctx, p, f.Cwd, nativePageSize, archived, pos.Cursor)
			if err != nil {
				return page{}, err
			}
			skip := min(pos.Skip, len(items))
			if skip < len(items) {
				return page{items[skip:], next, pos}, nil
			}
			if pos.Cursor != nil && next != nil && *pos.Cursor == *next {
				return page{}, errors.New("Codex returned a repeated session cursor")
			}
			pos.Cursor, pos.Skip, pos.Done = next, 0, next == nil
		}
		return page{pos: pos}, nil
	}
	active, err := pull(&state.Active, false)
	if err != nil {
		return nil, nil, err
	}
	archived, err := pull(&state.Archived, true)
	if err != nil {
		return nil, nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = nativePageSize
	}
	out := make([]backend.SessionInfo, 0, min(limit, 2*nativePageSize))
	// Stop after exhausting a native page if it has a successor. Its next
	// session could sort before the other page's remaining entries.
	for len(out) < limit && (len(active.items) > 0 || len(archived.items) > 0) {
		choice := &active
		if len(active.items) == 0 || (len(archived.items) > 0 && archived.items[0].UpdatedAt.After(active.items[0].UpdatedAt)) {
			choice = &archived
		}
		out = append(out, choice.items[0])
		choice.items = choice.items[1:]
		choice.pos.Skip++
		if len(choice.items) == 0 {
			choice.pos.Cursor, choice.pos.Skip, choice.pos.Done = choice.next, 0, choice.next == nil
			if !choice.pos.Done {
				break
			}
		}
	}
	if state.Active.Done && state.Archived.Done {
		return out, nil, nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, nil, err
	}
	return out, new(base64.RawURLEncoding.EncodeToString(raw)), nil
}

type selectionOptions struct {
	Provider  string
	Model     string
	Effort    string
	Profile   string
	Mode      string
	Models    []model
	Profiles  []profile
	Providers []provider
}

type provider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type profile struct {
	ID          string  `json:"id"`
	Allowed     bool    `json:"allowed"`
	Description *string `json:"description"`
}

func (p *process) profiles(ctx context.Context, cwd string) []profile {
	var result struct {
		Data []profile `json:"data"`
	}
	_ = p.call(ctx, "permissionProfile/list", object{"cwd": cwd}, &result)
	return result.Data
}

func (b *Backend) setupCatalog(ctx context.Context, p *process, cwd string, source *thread) (selectionOptions, error) {
	var result struct {
		Config struct {
			Model    string `json:"model"`
			Effort   string `json:"model_reasoning_effort"`
			Provider string `json:"model_provider"`
			Profile  string `json:"default_permissions"`
		} `json:"config"`
	}
	if err := p.call(ctx, "config/read", object{"cwd": cwd}, &result); err != nil {
		return selectionOptions{}, err
	}
	cfg := result.Config
	var catalog struct {
		Data    []provider `json:"data"`
		Default string     `json:"defaultProviderId"`
	}
	_ = p.call(ctx, "modelProvider/list", object{}, &catalog)
	def := cfg.Provider
	if def == "" {
		def = catalog.Default
	}
	state := selectionOptions{Provider: def, Model: cfg.Model, Effort: cfg.Effort, Profile: cfg.Profile,
		Profiles: p.profiles(ctx, cwd), Providers: catalog.Data}
	if source != nil {
		if source.Provider != "" {
			state.Provider = source.Provider
		}
		if source.Model != "" {
			state.Model = source.Model
		}
		if source.Effort != "" {
			state.Effort = source.Effort
		}
		settings, err := readTurnSettings(ctx, *source)
		if err != nil {
			return selectionOptions{}, err
		}
		if source.Model == "" && settings.Model != "" {
			state.Model = settings.Model
		}
		if settings.Permission != nil {
			state.Profile = settings.Permission.ID
		}
		if settings.Collaboration != nil {
			state.Mode = settings.Collaboration.Mode
			if source.Effort == "" && settings.Collaboration.Settings.Effort != "" {
				state.Effort = settings.Collaboration.Settings.Effort
			}
		}
		if source.Effort == "" && settings.Effort != "" {
			state.Effort = settings.Effort
		}
	}
	state.Models = p.models(ctx, state.Provider)
	state.fillDefaultModel()
	return state, nil
}

func (o *selectionOptions) fillDefaultModel() {
	if o.Model == "" {
		i := slices.IndexFunc(o.Models, func(m model) bool { return m.Default })
		if i < 0 && len(o.Models) != 0 {
			i = 0
		}
		if i >= 0 {
			o.Model, o.Effort = o.Models[i].ID, o.Models[i].Effort
		}
	}
}

func (o *selectionOptions) selectValue(id, v string) bool {
	switch id {
	case "model":
		i := slices.IndexFunc(o.Models, func(m model) bool { return m.ID == v })
		if i < 0 {
			return v != "" && v == o.Model
		}
		if v != o.Model {
			o.Model, o.Effort = v, o.Models[i].Effort
		}
	case "reasoning_effort":
		i := slices.IndexFunc(o.Models, func(m model) bool { return m.ID == o.Model })
		if i < 0 || !slices.ContainsFunc(o.Models[i].Efforts, func(e reasoningEffort) bool { return e.ID == v }) {
			return false
		}
		o.Effort = v
	case "mode":
		if v == "default" {
			o.Profile = ""
		} else if slices.ContainsFunc(o.Profiles, func(p profile) bool { return p.Allowed && p.ID == v }) {
			o.Profile = v
		} else {
			return false
		}
	default:
		return false
	}
	return true
}

func (b *Backend) applySetupConfig(ctx context.Context, p *process, cwd string, source *thread, entries []backend.ConfigSelection, params object) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	state, err := b.setupCatalog(ctx, p, cwd, source)
	if err != nil {
		return "", err
	}
	if model, ok := params["model"].(string); ok {
		state.Model = model
	}
	var mode string
	conf := object{}
	for _, entry := range entries {
		switch entry.ID {
		case "provider":
			if entry.Value == state.Provider || !slices.ContainsFunc(state.Providers, func(p provider) bool { return p.ID == entry.Value }) {
				continue
			}
			state.Provider, state.Model, state.Effort = entry.Value, "", ""
			state.Models = p.models(ctx, state.Provider)
			state.fillDefaultModel()
			params["modelProvider"] = state.Provider
			delete(params, "model")
			delete(conf, "model_reasoning_effort")
			if state.Model != "" {
				params["model"] = state.Model
			}
			if state.Effort != "" {
				conf["model_reasoning_effort"] = state.Effort
			}
		case "model":
			old := state.Model
			if state.selectValue(entry.ID, entry.Value) {
				params["model"] = state.Model
				if state.Model != old {
					delete(conf, "model_reasoning_effort")
					if state.Effort != "" {
						conf["model_reasoning_effort"] = state.Effort
					}
				}
			}
		case "reasoning_effort":
			if state.selectValue(entry.ID, entry.Value) {
				conf["model_reasoning_effort"] = state.Effort
			}
		case "mode":
			if entry.Value != "default" && state.selectValue(entry.ID, entry.Value) {
				params["permissions"] = state.Profile
			}
		case "collaboration_mode":
			if state.Model != "" && collaborationMode(entry.Value) {
				mode = entry.Value
			}
		}
	}
	if len(conf) != 0 {
		params["config"] = conf
	}
	return mode, nil
}
