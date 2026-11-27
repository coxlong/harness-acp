package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/coxlong/harness-acp/internal/backend"
)

func (b *Backend) SavedOptions(ctx context.Context, id, _ string, dirs []string) (backend.Options, error) {
	p, err := b.connect(ctx)
	if err != nil {
		return backend.Options{}, err
	}
	t, err := b.readThread(ctx, p, id, false)
	if err != nil || t.ParentID != "" {
		if err == nil {
			err = &backend.SessionNotFoundError{ID: id}
		}
		return backend.Options{}, err
	}
	if _, err := b.workspace(t.Cwd, dirs); err != nil {
		return backend.Options{}, err
	}
	state, err := b.setupCatalog(ctx, p, t.Cwd, &t)
	if err != nil {
		return backend.Options{}, err
	}
	skills, _ := p.skills(ctx, append([]string{t.Cwd}, dirs...))
	return sessionOptions(state.Model, state.Effort, state.Profile, state.Mode, state.Provider, state.Models, state.Profiles, skills), nil
}

type turnSettings struct {
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	Permission *struct {
		ID string `json:"id"`
	} `json:"active_permission_profile"`
	Collaboration *struct {
		Mode     string `json:"mode"`
		Settings struct {
			Effort string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
}

// thread/read supplies the rollout path and model metadata. The latest turn
// context supplies the permission profile and collaboration mode without
// starting a thread, loading its runtime, or connecting its tools.
func readTurnSettings(ctx context.Context, t thread) (turnSettings, error) {
	var settings turnSettings
	if t.Path == "" {
		return settings, nil
	}
	file, err := os.Open(t.Path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return settings, ctx.Err()
		}
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"turn_context"`)) {
			continue
		}
		var record struct {
			Type    string       `json:"type"`
			Payload turnSettings `json:"payload"`
		}
		if json.Unmarshal(line, &record) == nil && record.Type == "turn_context" {
			settings = record.Payload
		}
	}
	return settings, scanner.Err()
}
