package acp

import (
	"log"
	"time"
)

// AuditEvent records one auditable action.
type AuditEvent struct {
	Time       time.Time
	SessionID  string
	Agent      string
	Action     string // "prompt", "permission_allowed", "permission_denied", "session_new", "session_close"
	Details    string
	DurationMs int64
}

// Auditor writes structured audit log lines.
type Auditor struct{}

// NewAuditor returns a new Auditor.
func NewAuditor() *Auditor { return &Auditor{} }

// Log writes one audit event to the structured log.
func (a *Auditor) Log(ev AuditEvent) {
	log.Printf("AUDIT action=%s session=%s agent=%s details=%q duration_ms=%d",
		ev.Action, ev.SessionID, ev.Agent, ev.Details, ev.DurationMs)
}
