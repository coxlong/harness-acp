package session

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type eventHandle struct {
	backend.SessionHandle
	events chan backend.Event
}

func TestReplayIncludesLiveOutputAtItsOwnCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r := New(1)
	h := &eventHandle{events: make(chan backend.Event, 4)}
	r.Register("thread", "codex", h)
	t.Cleanup(func() { _ = r.Remove(context.Background(), "thread") })
	first, err := r.Attach("thread", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Unsubscribe()
	publish := func(child string, update acp2.SessionUpdate) {
		t.Helper()
		h.events <- backend.Event{Kind: backend.EventUpdate, Session: child, Update: update}
		select {
		case <-first.Events:
		case <-ctx.Done():
			t.Fatal("update not delivered")
		}
	}
	publish("worker", schema.NewSessionUpdate(schema.SessionUpdateAgentMessageChunk{
		MessageID: "message", Content: acp2.TextBlock("before"),
	}))
	publish("", schema.NewSessionUpdate(schema.SessionUpdateTerminalOutputChunk{
		TerminalID: "terminal", Data: base64.StdEncoding.EncodeToString([]byte{0x00, 0x01}),
	}))
	resumed, err := r.Attach("thread", nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Unsubscribe()
	publish("worker", schema.NewSessionUpdate(schema.SessionUpdateAgentMessageChunk{
		MessageID: "message", Content: acp2.TextBlock("after"),
	}))
	if len(resumed.LiveOutput) != 2 || resumed.LiveOutput[0].Session != "worker" {
		t.Fatalf("unexpected output snapshot: %+v", resumed.LiveOutput)
	}
	message := resumed.LiveOutput[0].Update.Variant().(schema.SessionUpdateAgentMessage)
	blocks, _ := message.Content.Get()
	if text := blocks[0].Variant().(schema.ContentBlockText).Text; text != "before" {
		t.Fatalf("message snapshot = %q, want before", text)
	}
	terminal := resumed.LiveOutput[1].Update.Variant().(schema.SessionUpdateTerminalUpdate)
	if output, _ := terminal.Output.Get(); output.Data != "AAE=" {
		t.Fatalf("terminal snapshot = %q, want AAE=", output.Data)
	}
	select {
	case next := <-resumed.Events:
		if next.Cursor.Seq != resumed.Cursor.Seq+1 || next.Session != "worker" {
			t.Fatalf("unexpected live update: %+v", next)
		}
	case <-ctx.Done():
		t.Fatal("missing continuation")
	}
}

func (h *eventHandle) Events() <-chan backend.Event { return h.events }
func (h *eventHandle) Close(context.Context) error {
	close(h.events)
	return nil
}

func TestReplayCheckpointAndInteractionRecipient(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r := New(2)
	h := &eventHandle{events: make(chan backend.Event, 10)}
	r.Register("thread", "codex", h)
	t.Cleanup(func() { _ = r.Remove(context.Background(), "thread") })
	attach := func(cursor *Cursor, form bool) Subscription {
		t.Helper()
		s, err := r.Attach("thread", cursor, form, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Unsubscribe)
		return s
	}
	read := func(s Subscription) Delivery {
		t.Helper()
		select {
		case d := <-s.Events:
			return d
		case <-ctx.Done():
			t.Fatal("update not delivered")
			return Delivery{}
		}
	}

	first := attach(nil, true)
	second := attach(nil, true)
	h.events <- backend.Event{Kind: backend.EventTurnState}
	checkpoint := read(first).Cursor
	if checkpoint.Seq != 1 || checkpoint.Generation == "" {
		t.Fatalf("invalid first checkpoint: %+v", checkpoint)
	}
	_ = read(second)
	third := attach(nil, false)
	h.events <- backend.Event{Kind: backend.EventElicitation, Elicitation: &backend.Elicitation{
		Context: ctx, Reply: func(*acp2.CreateElicitationResponse) {},
	}}
	if got := read(second); got.Kind != backend.EventElicitation || got.Cursor.Generation != "" {
		t.Fatalf("live interaction got a replay cursor: %+v", got)
	}
	select {
	case d := <-first.Events:
		t.Fatalf("older client received request: %+v", d)
	case d := <-third.Events:
		t.Fatalf("client without form support received request: %+v", d)
	default:
	}
	first.Unsubscribe()
	h.events <- backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{State: backend.TurnRunning}}
	_ = read(third)
	h.events <- backend.Event{Kind: backend.EventTurnState, Turn: backend.TurnEvent{State: backend.TurnIdle}}
	_ = read(third)

	resumed := attach(&checkpoint, true)
	if len(resumed.Replay) != 2 || resumed.Replay[0].Cursor.Seq != 2 || resumed.Replay[1].Cursor.Seq != 3 {
		t.Fatalf("unexpected replay: %+v", resumed.Replay)
	}
	expired := checkpoint
	expired.Seq = 0
	if _, err := r.Attach("thread", &expired, true, false); !errors.Is(err, ErrReplayGap) {
		t.Fatalf("old position got %v", err)
	}
	expired = checkpoint
	expired.Generation = "previous process"
	if _, err := r.Attach("thread", &expired, true, false); !errors.Is(err, ErrReplayGap) {
		t.Fatalf("wrong generation got %v", err)
	}
}

func TestUsageSnapshotAtReplayCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r := New(1)
	h := &eventHandle{events: make(chan backend.Event, 4)}
	r.Register("thread", "claude", h)
	t.Cleanup(func() { _ = r.Remove(context.Background(), "thread") })
	attach := func(fromStart bool) Subscription {
		t.Helper()
		sub, err := r.Attach("thread", nil, false, fromStart)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sub.Unsubscribe)
		return sub
	}
	first := attach(false)
	read := func(sub Subscription) Delivery {
		t.Helper()
		select {
		case d := <-sub.Events:
			return d
		case <-ctx.Done():
			t.Fatal("update not delivered")
			return Delivery{}
		}
	}
	publish := func(child string, usage schema.SessionUpdateUsageUpdate) {
		h.events <- backend.Event{
			Kind: backend.EventUpdate, Session: child, Update: schema.NewSessionUpdate(usage),
		}
		read(first)
	}

	cost := schema.Cost{Amount: 1, Currency: "USD"}
	publish("", schema.SessionUpdateUsageUpdate{Used: 60, Size: 100, Cost: optional.Of(cost)})
	publish("", schema.SessionUpdateUsageUpdate{Used: 30, Size: 100})
	publish("child", schema.SessionUpdateUsageUpdate{Used: 90, Size: 100})
	resumed := attach(true)
	if got, ok := resumed.Usage.Cost.Get(); !ok || got.Amount != cost.Amount || got.Currency != cost.Currency {
		t.Fatalf("snapshot cost = %+v, present = %t", got, ok)
	}
	if resumed.Usage.Used != 30 || resumed.Usage.Size != 100 {
		t.Fatalf("unexpected usage snapshot: %+v", resumed.Usage)
	}
	publish("", schema.SessionUpdateUsageUpdate{Used: 40, Size: 100, Cost: optional.Null[schema.Cost]()})
	update := read(resumed)
	if update.Cursor.Seq != resumed.Cursor.Seq+1 || update.Update.Variant().(schema.SessionUpdateUsageUpdate).Used != 40 {
		t.Fatalf("unexpected live update after usage snapshot: %+v", update)
	}
	if next := attach(true); !next.Usage.Cost.IsNull() {
		t.Fatalf("snapshot did not preserve the cost clear: %+v", next.Usage)
	}
}
