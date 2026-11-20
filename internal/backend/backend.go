package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/ironpark/acp-go/acp2"
)

// InvalidParamsError marks a client-input problem: a parameter value the
// backend cannot honour. The ACP facade reports it as JSON-RPC -32602
// (Invalid params), as the protocol assigns malformed and unsupported values.
type InvalidParamsError struct{ Detail string }

func (e *InvalidParamsError) Error() string { return e.Detail }

// InvalidParams builds an InvalidParamsError, printf-style.
func InvalidParams(format string, a ...any) error {
	return &InvalidParamsError{Detail: fmt.Sprintf(format, a...)}
}

// SessionNotFoundError marks a session the backend does not have. The facade
// reports it as -32002 (Resource not found) with data.sessionId, which the
// protocol requires for session/resume of a session that does not exist.
type SessionNotFoundError struct{ ID string }

func (e *SessionNotFoundError) Error() string { return "session " + e.ID + " not found" }

// CwdNotExistError marks a working directory that is gone on this machine.
// The parameter cannot be honoured, so the facade reports it as -32602 with
// a message that names the directory, instead of a spawn failure.
type CwdNotExistError struct{ Path string }

func (e *CwdNotExistError) Error() string {
	return "working directory " + e.Path + " no longer exists"
}

// AgentInfo describes a backend agent.
type AgentInfo struct {
	Name        string
	DisplayName string
	Version     string
	Available   bool
}

// ListFilter selects one page of session list results.
type ListFilter struct {
	Cwd    string // only sessions of exactly this directory; empty for all
	Offset int    // sessions to skip, newest first
	Limit  int    // page size; 0 for no limit
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
	ID        string
	Cwd       string
	Title     string
	UpdatedAt time.Time // last activity
}

// Options is what a client can configure and invoke in a session: config
// options (mode, model, effort, ...) and slash commands.
type Options struct {
	Config   []acp2.SessionConfigOption
	Commands []acp2.AvailableCommand
}

// HistoryUpdate is one replayed update: of the session itself, or of a child
// session (a subagent's) when Session names it.
type HistoryUpdate struct {
	Session string
	Update  acp2.SessionUpdate
}

// ReplayedChild is a subagent whose history was replayed.
type ReplayedChild struct {
	ID     string // its session
	Parent string // the session that launched it; "" for the replayed session itself
	// End is how its work ended: an idle state with a stop reason or an error,
	// or with neither when the history does not say.
	End TurnEvent
}

// History is a session's replay: its updates in order, with those of its
// subagents' child sessions among them, each after the call that launched the
// subagent.
type History struct {
	Updates []HistoryUpdate
	// Children are the subagents the replay announced, in announcement order.
	Children []ReplayedChild
	// Missing counts the subagents whose history could not be restored.
	Missing int
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
	// Background is the complete set of work running apart from the foreground
	// turn. nil means the backend is not reporting it; empty means none runs.
	Background []acp2.BackgroundWork
}

// PermissionRequest is a pending tool-use authorisation.
type PermissionRequest struct {
	ReqID      string
	Session    string // the child session (a subagent's) that asks; "" for the session itself
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
	// Session is the child session (a subagent's) the event belongs to; empty
	// for the session itself.
	Session    string
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
	// ListSessions returns one page, newest first, and whether more follow it.
	ListSessions(ctx context.Context, f ListFilter) (sessions []SessionInfo, more bool, err error)
	// History returns entity-level snapshots for replayFrom:start.
	// Each update is already a complete entity state, not a raw delta.
	History(ctx context.Context, id string) (History, error)
	// Options reports what a session started in cwd could be configured with,
	// without creating one.
	Options(ctx context.Context, cwd string, additionalDirs []string) (Options, error)
	NewSession(ctx context.Context, p NewSessionParams) (SessionHandle, error)
	ResumeSession(ctx context.Context, id string, p ResumeParams) (SessionHandle, error)
	// DeleteSession removes a session and its stored history. A session that
	// is not there is not an error: the caller's goal is that it is gone.
	// An id the backend cannot accept is an InvalidParamsError.
	DeleteSession(ctx context.Context, id string) error
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
	// Options returns the session's current options.
	Options() Options
	// SetConfigOption changes one option and returns all of them. value is a
	// string for a select option and a bool for a boolean one. A change is also
	// reported to subscribers as a config_option_update.
	SetConfigOption(ctx context.Context, id string, value any) ([]acp2.SessionConfigOption, error)
	Cancel(ctx context.Context) error
	Close(ctx context.Context) error
}
