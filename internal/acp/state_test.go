package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func wire(t *testing.T, e backend.TurnEvent) string {
	t.Helper()
	b, err := json.Marshal(stateUpdate(e))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// No background list means "not reported"; an empty one means "nothing runs".
func TestStateUpdateBackground(t *testing.T) {
	if got := wire(t, backend.TurnEvent{State: backend.TurnRunning}); strings.Contains(got, "background") {
		t.Errorf("unreported background was sent: %s", got)
	}
	empty := wire(t, backend.TurnEvent{State: backend.TurnRunning, Background: []acp2.BackgroundWork{}})
	if !strings.Contains(empty, `"background":[]`) {
		t.Errorf("empty background = %s", empty)
	}
	one := wire(t, backend.TurnEvent{State: backend.TurnIdle, StopReason: acp2.StopReasonEndTurn, Background: []acp2.BackgroundWork{
		schema.NewBackgroundWork(schema.BackgroundWorkTerminal{TerminalID: "t1"}),
	}})
	if !strings.Contains(one, `"stopReason":"end_turn"`) || !strings.Contains(one, `"terminalId":"t1"`) {
		t.Errorf("idle with a terminal = %s", one)
	}
	// A snapshot of an idle session is no completion: it names no stop reason.
	if snap := wire(t, backend.TurnEvent{State: backend.TurnIdle}); strings.Contains(snap, "stopReason") {
		t.Errorf("idle snapshot = %s", snap)
	}
}
