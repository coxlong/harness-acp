package acp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// fakeLister serves an in-memory list, newest first, with the backend's paging contract.
type fakeLister struct {
	backend.Backend
	all []backend.SessionInfo
}

func (f fakeLister) ListSessions(_ context.Context, fl backend.ListFilter) ([]backend.SessionInfo, bool, error) {
	var in []backend.SessionInfo
	for _, s := range f.all {
		if fl.Cwd == "" || s.Cwd == fl.Cwd {
			in = append(in, s)
		}
	}
	if fl.Offset >= len(in) {
		return nil, false, nil
	}
	in = in[fl.Offset:]
	if fl.Limit > 0 && len(in) > fl.Limit {
		return in[:fl.Limit], true, nil
	}
	return in, false, nil
}

func TestListSessionsWire(t *testing.T) {
	var all []backend.SessionInfo
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 1500; i++ {
		all = append(all, backend.SessionInfo{ID: fmt.Sprintf("s%04d", i), Cwd: "/w", Title: "t", UpdatedAt: now.Add(-time.Duration(i) * time.Second)})
	}
	ctx := context.Background()
	_, client := acp2.Pipe(ctx,
		func(c *acp2.AgentSideConnection) acp2.Agent {
			return NewFacade("claude", fakeLister{all: all}, session.New(10), nil, c)
		},
		func(*acp2.ClientSideConnection) acp2.Client { return newRecorder() },
	)
	if _, err := client.Initialize(ctx, &acp2.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}

	p1, err := client.ListSessions(ctx, &acp2.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Sessions) != 1000 || p1.NextCursor == nil || string(*p1.NextCursor) != "offset:1000" {
		t.Fatalf("page 1: %d sessions, cursor %v", len(p1.Sessions), p1.NextCursor)
	}
	if u := p1.Sessions[0].UpdatedAt; u == nil || *u != "2026-11-20T09:00:00Z" {
		t.Errorf("updatedAt = %v", u)
	}
	p2, err := client.ListSessions(ctx, &acp2.ListSessionsRequest{Cursor: p1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Sessions) != 500 || p2.NextCursor != nil || p2.Sessions[0].SessionID != "s1000" {
		t.Fatalf("page 2: %d sessions first %s cursor %v", len(p2.Sessions), p2.Sessions[0].SessionID, p2.NextCursor)
	}

	bad := schema.SessionListCursor("nonsense")
	if _, err := client.ListSessions(ctx, &acp2.ListSessionsRequest{Cursor: &bad}); err == nil {
		t.Error("an unknown cursor must be refused")
	}
	rel := schema.AbsolutePath("relative/dir")
	if _, err := client.ListSessions(ctx, &acp2.ListSessionsRequest{Cwd: &rel}); err == nil {
		t.Error("a relative cwd must be refused")
	}
	none := schema.AbsolutePath("/elsewhere")
	if r, err := client.ListSessions(ctx, &acp2.ListSessionsRequest{Cwd: &none}); err != nil || len(r.Sessions) != 0 {
		t.Errorf("cwd filter: %v %v", r, err)
	}
}
