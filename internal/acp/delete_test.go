package acp

import (
	"context"
	"errors"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
)

// deleteBackend records what the facade asked it to delete.
type deleteBackend struct {
	backend.Backend
	deleted []string
	err     error
}

func (d *deleteBackend) DeleteSession(_ context.Context, id string) error {
	d.deleted = append(d.deleted, id)
	return d.err
}

// wiredInit connects a facade and returns both the client and its handshake,
// so a test can read the capabilities the agent declared.
func wiredInit(t *testing.T, be backend.Backend) (*acp2.ClientSideConnection, *acp2.InitializeResponse) {
	t.Helper()
	ctx := context.Background()
	_, client := acp2.Pipe(ctx,
		func(c *acp2.AgentSideConnection) acp2.Agent {
			return NewFacade("claude", be, session.New(), nil, c)
		},
		func(*acp2.ClientSideConnection) acp2.Client { return newRecorder() },
	)
	init, err := client.Initialize(ctx, &acp2.InitializeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return client, init
}

// Implementing session/delete is how the agent advertises it: the capability
// and the handler come from the same method.
func TestDeleteSessionIsAdvertised(t *testing.T) {
	_, init := wiredInit(t, &deleteBackend{})
	if init.Capabilities.Session == nil || init.Capabilities.Session.Delete == nil {
		t.Fatalf("capabilities = %+v, want session.delete", init.Capabilities.Session)
	}
}

// Deleting an unknown session succeeds and reaches the backend, which decides
// whether the session was there.
func TestDeleteSessionSucceeds(t *testing.T) {
	be := &deleteBackend{}
	client, _ := wiredInit(t, be)
	if _, err := client.DeleteSession(context.Background(), &acp2.DeleteSessionRequest{
		SessionID: "gone",
	}); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if len(be.deleted) != 1 || be.deleted[0] != "gone" {
		t.Errorf("backend deleted %v, want [gone]", be.deleted)
	}
}

// Backend failures keep their protocol meaning: a bad id is invalid params, and
// anything else is internal.
func TestDeleteSessionErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code acp.ErrorCode
	}{
		{"invalid params", backend.InvalidParams("invalid session id %q", "../x"), acp.ErrorCodeInvalidParams},
		{"internal", errors.New("disk on fire"), acp.ErrorCodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &deleteBackend{err: tc.err}
			client, _ := wiredInit(t, be)
			_, err := client.DeleteSession(context.Background(), &acp2.DeleteSessionRequest{
				SessionID: "../x",
			})
			re := requestError(t, err)
			if re.Code != tc.code {
				t.Errorf("code = %d, want %d", re.Code, tc.code)
			}
			if tc.code == acp.ErrorCodeInternalError && re.Message == "" {
				t.Error("internal error has no message")
			}
		})
	}
}
