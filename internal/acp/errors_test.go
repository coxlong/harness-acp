package acp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/coxlong/harness-acp/internal/session"
	"github.com/ironpark/acp-go"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// failingBackend answers session/new and session/resume with a chosen error,
// so the facade's wire codes can be checked without spawning anything.
type failingBackend struct {
	backend.Backend
	newErr    error
	resumeErr error
}

func (f failingBackend) NewSession(context.Context, backend.NewSessionParams) (backend.SessionHandle, error) {
	if f.newErr == nil {
		return nil, errors.New("fake backend: session/new not configured")
	}
	return nil, f.newErr
}

func (f failingBackend) ResumeSession(context.Context, string, backend.ResumeParams) (backend.SessionHandle, error) {
	if f.resumeErr == nil {
		return nil, errors.New("fake backend: session/resume not configured")
	}
	return nil, f.resumeErr
}

// wiredClient connects a facade over a fake backend and returns its client.
func wiredClient(t *testing.T, be backend.Backend, allowDirs []string) *acp2.ClientSideConnection {
	t.Helper()
	ctx := context.Background()
	_, client := acp2.Pipe(ctx,
		func(c *acp2.AgentSideConnection) acp2.Agent {
			return NewFacade("claude", be, session.New(), allowDirs, c)
		},
		func(*acp2.ClientSideConnection) acp2.Client { return newRecorder() },
	)
	if _, err := client.Initialize(ctx, &acp2.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}
	return client
}

func requestError(t *testing.T, err error) *acp.RequestError {
	t.Helper()
	var re *acp.RequestError
	if !errors.As(err, &re) {
		t.Fatalf("error is not a RequestError: %v", err)
	}
	return re
}

type setupBackend struct {
	failingBackend
}

func (setupBackend) SavedOptions(_ context.Context, id, _ string, _ []string) (backend.Options, error) {
	return backend.Options{}, &backend.SessionNotFoundError{ID: id}
}

func TestInitialConfigRejectsUnsupportedTypesAndDuplicates(t *testing.T) {
	selectModel := schema.NewInitialSessionConfigOption(schema.InitialSessionConfigOptionID{ConfigID: "model", Value: "x"})
	boolean := schema.NewInitialSessionConfigOption(schema.InitialSessionConfigOptionBoolean{ConfigID: "flag", Value: true})
	client := wiredClient(t, setupBackend{}, nil)
	for _, entries := range [][]schema.InitialSessionConfigOption{
		{boolean}, {selectModel, selectModel},
	} {
		_, err := client.NewSession(t.Context(), &acp2.NewSessionRequest{Cwd: "/w", ConfigOptions: entries})
		if !acp.IsCode(err, acp.ErrorCodeInvalidParams) {
			t.Fatalf("session/new: got %v, want invalid params", err)
		}
	}
}

func TestOptionsForMissingSessionReturnsResourceNotFound(t *testing.T) {
	client := wiredClient(t, setupBackend{}, nil)
	_, err := client.GetSessionOptions(t.Context(), &acp2.SessionOptionsRequest{Cwd: "/w", SessionID: new(schema.SessionID("ghost"))})
	if id, ok := acp.SessionNotFoundID(err); !ok || id != "ghost" {
		t.Fatalf("session/options: got %v, want missing session ghost", err)
	}
}

// A working directory that no longer exists is a parameter the agent cannot
// honour: -32602, with a message that names the directory instead of blaming
// the binary.
func TestResumeCwdGoneIsInvalidParams(t *testing.T) {
	client := wiredClient(t, failingBackend{
		resumeErr: &backend.CwdNotExistError{Path: "/tmp/gone/001"},
	}, nil)
	_, err := client.ResumeSession(context.Background(), &acp2.ResumeSessionRequest{
		SessionID: "s1",
		Cwd:       "/tmp/gone/001",
	})
	re := requestError(t, err)
	if !acp.IsCode(err, acp.ErrorCodeInvalidParams) {
		t.Errorf("code = %d, want -32602", re.Code)
	}
	if !strings.Contains(re.Message, "/tmp/gone/001") || !strings.Contains(re.Message, "no longer exists") {
		t.Errorf("message = %q, want the missing directory named", re.Message)
	}
}

// Resuming a session the backend does not have is the protocol's unknown
// session: -32002 with data.sessionId.
func TestResumeUnknownSessionIsResourceNotFound(t *testing.T) {
	client := wiredClient(t, failingBackend{
		resumeErr: &backend.SessionNotFoundError{ID: "nope"},
	}, nil)
	_, err := client.ResumeSession(context.Background(), &acp2.ResumeSessionRequest{
		SessionID: "nope",
		Cwd:       "/w",
	})
	if !acp.IsCode(err, acp.ErrorCodeResourceNotFound) {
		t.Fatalf("code = %v, want -32002", requestError(t, err).Code)
	}
	if id, ok := acp.SessionNotFoundID(err); !ok || id != "nope" {
		t.Errorf("data.sessionId = %q, %v; want nope", id, ok)
	}
}

// Any other backend failure keeps the internal class, with the operation name.
func TestResumeOtherFailureIsInternal(t *testing.T) {
	client := wiredClient(t, failingBackend{
		resumeErr: errors.New("claude spawn: stdin pipe: broken"),
	}, nil)
	_, err := client.ResumeSession(context.Background(), &acp2.ResumeSessionRequest{
		SessionID: "s1",
		Cwd:       "/w",
	})
	re := requestError(t, err)
	if !acp.IsCode(err, acp.ErrorCodeInternalError) {
		t.Errorf("code = %d, want -32603", re.Code)
	}
	if !strings.Contains(re.Message, "resume session") {
		t.Errorf("message = %q, want the operation named", re.Message)
	}
}

// Prompting a session that is not active on the connection is also an unknown
// session: -32002 with data.sessionId, not an internal error.
func TestPromptInactiveSessionIsResourceNotFound(t *testing.T) {
	client := wiredClient(t, failingBackend{}, nil)
	_, err := client.Prompt(context.Background(), &acp2.PromptRequest{
		SessionID: "ghost",
		Prompt:    []acp2.ContentBlock{acp2.TextBlock("hi")},
	})
	if !acp.IsCode(err, acp.ErrorCodeResourceNotFound) {
		t.Fatalf("code = %v, want -32002", requestError(t, err).Code)
	}
	if id, ok := acp.SessionNotFoundID(err); !ok || id != "ghost" {
		t.Errorf("data.sessionId = %q, %v; want ghost", id, ok)
	}
}

// The facade's own parameter checks keep -32602 on the wire.
func TestFacadeParamChecksAreInvalidParams(t *testing.T) {
	client := wiredClient(t, failingBackend{}, []string{"/w"})
	ctx := context.Background()

	_, err := client.NewSession(ctx, &acp2.NewSessionRequest{Cwd: "/elsewhere"})
	if !acp.IsCode(err, acp.ErrorCodeInvalidParams) {
		t.Errorf("new outside roots: code = %v, want -32602", requestError(t, err).Code)
	}

	cursor := schema.SessionListCursor("junk")
	_, err = client.ListSessions(ctx, &acp2.ListSessionsRequest{Cursor: &cursor})
	if !acp.IsCode(err, acp.ErrorCodeInvalidParams) {
		t.Errorf("list cursor: code = %v, want -32602", requestError(t, err).Code)
	}

	cwd := schema.AbsolutePath("relative/path")
	_, err = client.ListSessions(ctx, &acp2.ListSessionsRequest{Cwd: &cwd})
	if !acp.IsCode(err, acp.ErrorCodeInvalidParams) {
		t.Errorf("relative cwd: code = %v, want -32602", requestError(t, err).Code)
	}
}

// The unknown session is answered before the replay cursor is parsed: a
// resume of a session that does not exist is -32002 whatever the cursor says.
func TestResumeUnknownSessionWinsOverCursor(t *testing.T) {
	client := wiredClient(t, failingBackend{
		resumeErr: &backend.SessionNotFoundError{ID: "ghost"},
	}, nil)
	_, err := client.ResumeSession(context.Background(), &acp2.ResumeSessionRequest{
		SessionID:  "ghost",
		Cwd:        "/w",
		ReplayFrom: schema.NewReplayFrom(schema.ReplayFromCustom{Type: "mystery"}),
	})
	if !acp.IsCode(err, acp.ErrorCodeResourceNotFound) {
		t.Fatalf("code = %v, want -32002", requestError(t, err).Code)
	}
	if id, ok := acp.SessionNotFoundID(err); !ok || id != "ghost" {
		t.Errorf("data.sessionId = %q, %v; want ghost", id, ok)
	}
}
