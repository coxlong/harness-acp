package codex

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

func TestPromptOrdersEarlyEventsAndRoutesApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), rpcTimeout)
	defer cancel()
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 8),
		pending: make(map[uint64]chan message), sessions: make(map[string]*Session),
	}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1"}})
	if _, err := p.put(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.end(nil) })

	type sent struct {
		id  acp2.MessageID
		err error
	}
	result := make(chan sent, 1)
	go func() {
		id, err := s.Send(ctx, []acp2.ContentBlock{acp2.TextBlock("hello")})
		result <- sent{id, err}
	}()

	readWire := func() message {
		t.Helper()
		select {
		case line := <-p.writes:
			var m message
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatal(err)
			}
			return m
		case <-ctx.Done():
			t.Fatal("timed out waiting for native RPC")
			return message{}
		}
	}
	request := readWire()
	if request.Method != "turn/start" {
		t.Fatalf("unexpected method %q", request.Method)
	}

	p.dispatch(message{
		Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}`),
	})
	p.dispatch(message{
		ID: json.RawMessage(`"approval-1"`), Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"cmd-1","command":"printf ok","availableDecisions":["accept","decline","cancel"]}`),
	})
	p.deliverReply(message{
		ID: request.ID, Result: json.RawMessage(`{"turn":{"id":"turn-1","status":"inProgress"}}`),
	})
	var out sent
	select {
	case out = <-result:
	case <-ctx.Done():
		t.Fatal("timed out waiting for prompt acceptance")
	}
	if out.err != nil {
		t.Fatal(out.err)
	}
	readEvent := func() backend.Event {
		t.Helper()
		select {
		case event := <-s.Events():
			return event
		case <-ctx.Done():
			t.Fatal("timed out waiting for ACP event")
			return backend.Event{}
		}
	}
	user, ok := readEvent().Update.Variant().(schema.SessionUpdateUserMessage)
	if !ok || user.MessageID != out.id {
		t.Fatalf("first event = %#v, want accepted user message %q", user, out.id)
	}
	if e := readEvent(); e.Kind != backend.EventTurnState || e.Turn.State != backend.TurnRunning {
		t.Fatalf("expected running, got %#v", e)
	}
	if e := readEvent(); e.Kind != backend.EventTurnState || e.Turn.State != backend.TurnRequiresAction {
		t.Fatalf("expected requires_action, got %#v", e)
	}
	e := readEvent()
	if e.Kind != backend.EventPermission || e.Permission.ToolCallID != "cmd-1" {
		t.Fatalf("expected request for cmd-1, got %#v", e)
	}
	if err := s.Respond(e.Permission.ReqID, backend.Decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	reply := readWire()
	if string(reply.ID) != `"approval-1"` || string(reply.Result) != `{"decision":"accept"}` {
		t.Fatalf("unexpected approval reply: %#v", reply)
	}
	if e := readEvent(); e.Kind != backend.EventTurnState || e.Turn.State != backend.TurnRunning {
		t.Fatalf("expected running after approval, got %#v", e)
	}
}

func TestInterruptWhileNativeTurnIsStarting(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), rpcTimeout)
	defer cancel()
	p := &process{
		done: make(chan struct{}), writes: make(chan []byte, 2),
		pending: make(map[uint64]chan message),
	}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1"}})
	s.startTurn("turn-1")
	t.Cleanup(func() { s.end(nil) })

	result := make(chan error, 1)
	go func() { result <- s.Cancel(ctx) }()
	for attempt := range 2 {
		select {
		case raw := <-p.writes:
			var msg message
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Method != "turn/interrupt" || string(msg.Params) != `{"threadId":"thread-1","turnId":"turn-1"}` {
				t.Fatalf("unexpected interrupt: %#v", msg)
			}
			if attempt == 0 {
				p.deliverReply(message{ID: msg.ID, Error: &rpcError{Code: -32600, Message: "no active turn to interrupt"}})
			} else {
				p.deliverReply(message{ID: msg.ID, Result: json.RawMessage(`{}`)})
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for native interrupt")
		}
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for cancel")
	}
}

func TestUserInputAnswersAndNativeCancellation(t *testing.T) {
	p := &process{done: make(chan struct{}), writes: make(chan []byte, 4)}
	s := newSession(p, threadResponse{Thread: thread{ID: "thread-1"}})
	s.startTurn("turn-1")
	<-s.Events()
	t.Cleanup(func() { s.end(nil) })

	input := message{
		ID: json.RawMessage("9"), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"itemId":"question","isBlocking":true,"questions":[{"id":"language","question":"Which language?","isOther":true,"options":[{"label":"Go"}]}]}`),
	}
	s.receive(input)
	if e := <-s.Events(); e.Turn.State != backend.TurnRequiresAction {
		t.Fatalf("expected blocked turn, got %+v", e)
	}
	ev := <-s.Events()
	req, err := ev.Elicitation.Request.As[schema.CreateElicitationRequestFormSession]()
	if err != nil || req.SessionID != "thread-1" || req.ToolCallID == nil || *req.ToolCallID != "question" {
		t.Fatalf("unexpected form: %+v %v", req, err)
	}
	choice, _ := schema.NewElicitationContentValue("None of the above")
	note, _ := schema.NewElicitationContentValue("Dart")
	response := schema.NewCreateElicitationResponse(schema.CreateElicitationResponseAccept{
		Content: map[string]schema.ElicitationContentValue{"language": choice, "language_note": note},
	})
	ev.Elicitation.Reply(&response)
	var reply message
	if err := json.Unmarshal(<-p.writes, &reply); err != nil {
		t.Fatal(err)
	}
	if string(reply.Result) != `{"answers":{"language":{"answers":["None of the above","user_note: Dart"]}}}` {
		t.Fatalf("unexpected answer: %s", reply.Result)
	}
	if e := <-s.Events(); e.Turn.State != backend.TurnRunning {
		t.Fatalf("expected resumed turn, got %+v", e)
	}

	s.receive(input)
	<-s.Events()
	ev = <-s.Events()
	s.receive(message{Method: "serverRequest/resolved", Params: json.RawMessage(`{"requestId":9}`)})
	if ev.Elicitation.Context.Err() == nil {
		t.Fatal("native cancellation did not withdraw form")
	}
	ev.Elicitation.Reply(&response)
	select {
	case late := <-p.writes:
		t.Fatalf("answered an expired request: %s", late)
	default:
	}
}

func TestSecretQuestionDoesNotOpenForm(t *testing.T) {
	form, fields := inputForm([]inputQuestion{{ID: "token", IsSecret: true}})
	if len(fields) != 0 || len(form.Properties) != 0 {
		t.Fatal("sensitive fields must not be requested over ACP form mode")
	}
}
