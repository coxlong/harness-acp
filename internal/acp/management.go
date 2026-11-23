package acp

import (
	"context"
	"fmt"
	"strings"

	"github.com/coxlong/harness-acp/internal/backend"
	acp "github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func initialConfig(entries []schema.InitialSessionConfigOption) ([]backend.ConfigSelection, error) {
	seen := make(map[string]bool)
	var out []backend.ConfigSelection
	for _, entry := range entries {
		var id string
		switch v := entry.Variant().(type) {
		case schema.InitialSessionConfigOptionID:
			id = string(v.ConfigID)
			if v.Value == "" {
				return nil, acp.InvalidParams("initial config value is empty")
			}
			out = append(out, backend.ConfigSelection{ID: id, Value: string(v.Value)})
		case schema.InitialSessionConfigOptionBoolean:
			return nil, acp.InvalidParams("initial configuration does not support boolean values")
		case schema.InitialSessionConfigOptionCustom:
			return nil, acp.InvalidParams("unsupported initial config value type")
		default:
			return nil, acp.InvalidParams("invalid initial config option")
		}
		if id == "" || seen[id] {
			return nil, acp.InvalidParams(fmt.Sprintf("invalid or repeated initial config option %q", id))
		}
		seen[id] = true
	}
	return out, nil
}

func (f *Facade) ForkSession(ctx context.Context, req *acp2.ForkSessionRequest) (*acp2.ForkSessionResponse, error) {
	be, ok := f.be.(backend.SessionManager)
	if !ok {
		return nil, acp.MethodNotFound("session/fork")
	}
	if !cwdAllowed(string(req.Cwd), f.allowDirs) {
		return nil, acp.InvalidParams("fork working directory is outside the allowed roots")
	}
	initial, err := f.initialConfig(req.ConfigOptions)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, d := range req.AdditionalDirectories {
		dirs = append(dirs, string(d))
	}
	h, err := be.ForkSession(ctx, string(req.SessionID), backend.NewSessionParams{
		Cwd: string(req.Cwd), AdditionalDirectories: dirs, Config: initial,
	})
	if err != nil {
		return nil, backendError(err, "fork session")
	}
	id := h.NativeID()
	f.reg.Register(id, f.name, h)
	_, start, err := f.attach(id, nil, false)
	if err != nil {
		return nil, err
	}
	start()
	opts := h.Options()
	return &acp2.ForkSessionResponse{SessionID: acp2.SessionID(id), ConfigOptions: opts.Config, AvailableCommands: opts.Commands}, nil
}

func (f *Facade) ArchiveSession(ctx context.Context, req *acp2.ArchiveSessionRequest) (*acp2.ArchiveSessionResponse, error) {
	be, ok := f.be.(backend.SessionManager)
	if !ok {
		return nil, acp.MethodNotFound("session/archive")
	}
	id := string(req.SessionID)
	unlock, err := f.reg.LockLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := f.closeSession(ctx, id); err != nil {
		return nil, err
	}
	if err := be.ArchiveSession(ctx, id, true); err != nil {
		return nil, backendError(err, "archive session")
	}
	return &acp2.ArchiveSessionResponse{}, nil
}

func (f *Facade) UnarchiveSession(ctx context.Context, req *acp2.UnarchiveSessionRequest) (*acp2.UnarchiveSessionResponse, error) {
	be, ok := f.be.(backend.SessionManager)
	if !ok {
		return nil, acp.MethodNotFound("session/unarchive")
	}
	id := string(req.SessionID)
	unlock, err := f.reg.LockLifecycle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := be.ArchiveSession(ctx, id, false); err != nil {
		return nil, backendError(err, "unarchive session")
	}
	return &acp2.UnarchiveSessionResponse{}, nil
}

func (f *Facade) SetSessionTitle(ctx context.Context, req *acp2.SetSessionTitleRequest) (*acp2.SetSessionTitleResponse, error) {
	be, ok := f.be.(backend.SessionManager)
	if !ok {
		return nil, acp.MethodNotFound("session/set_title")
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, acp.InvalidParams("session title cannot be empty")
	}
	if err := be.SetSessionTitle(ctx, string(req.SessionID), title); err != nil {
		return nil, backendError(err, "set session title")
	}
	return &acp2.SetSessionTitleResponse{}, nil
}
