package backend

import (
	"context"
	"github.com/ironpark/acp-go/acp2"
)

// AgentInfo describes a backend agent.
type AgentInfo struct {
	Name        string
	DisplayName string
	Version     string
	Available   bool
}

// ListFilter filters session list results.
type ListFilter struct {
	Cwd string
}

// NewSessionParams carries session/new parameters.
type NewSessionParams struct {
	Cwd                   string
	AdditionalDirectories []string
	Model                 string
	Instructions          string
}

// ResumeParams carries session/resume parameters.
type ResumeParams struct {
	Cwd                   string
	AdditionalDirectories []string
}

// SessionInfo describes a session for listing.
type SessionInfo struct {
	ID    string
	Cwd   string
	Title string
}

// EventKind labels events coming from a SessionHandle.
type EventKind int

const (
	EventUpdate     EventKind = iota // content update: Update is set
	EventTurnState                   // foreground state change: Turn is set
	EventPermission                  // pending tool authorisation: Permission is set
	EventFatal                       // session is dead: Err is set
)

// TurnState is the session's foreground state (ACP v2 state_update).
type TurnState int

const (
	// TurnIdle is the zero value: a session with no open turn. An idle event
	// ends a turn; StopReason or Err says how.
	TurnIdle TurnState = iota
	TurnRunning
	TurnRequiresAction
)

// TurnEvent reports a foreground state change. For TurnIdle, Err != nil means
// the work failed (ACP stopReason "error"); otherwise StopReason is set.
type TurnEvent struct {
	State      TurnState
	StopReason acp2.StopReason
	Err        error
}

// PermissionRequest is a pending tool-use authorisation.
type PermissionRequest struct {
	ReqID      string
	ToolCallID string // the tool call this request is about
	Title      string
	ToolName   string
	Input      map[string]any
}

// Decision is the result of a permission request.
type Decision struct {
	Allow bool
	// Cancelled means the turn was cancelled while the request was open: the
	// tool use is aborted rather than refused.
	Cancelled bool
}

// Event is one item from SessionHandle.Events().
type Event struct {
	Kind       EventKind
	Update     acp2.SessionUpdate // Kind == EventUpdate
	Turn       TurnEvent          // Kind == EventTurnState
	Permission PermissionRequest  // Kind == EventPermission
	Err        error              // Kind == EventFatal
}

// Backend is a process-level singleton for one agent type.
type Backend interface {
	Name() string
	Probe(ctx context.Context) (AgentInfo, error)
	ListSessions(ctx context.Context, f ListFilter) ([]SessionInfo, string, error)
	// History returns entity-level snapshots for replayFrom:start.
	// Each update is already a complete entity state, not a raw delta.
	History(ctx context.Context, id string) ([]acp2.SessionUpdate, error)
	NewSession(ctx context.Context, p NewSessionParams) (SessionHandle, error)
	ResumeSession(ctx context.Context, id string, p ResumeParams) (SessionHandle, error)
	Close() error
}

// SessionHandle is a session-level handle.
type SessionHandle interface {
	NativeID() string
	// Send submits a user message. If a turn is idle it starts one;
	// if one is running the message is steered into it.
	// Returns the ACP message ID.
	Send(ctx context.Context, blocks []acp2.ContentBlock) (acp2.MessageID, error)
	// Events returns the channel of events from this session.
	Events() <-chan Event
	// Respond answers a pending permission request.
	Respond(reqID string, d Decision) error
	Cancel(ctx context.Context) error
	Close(ctx context.Context) error
}
