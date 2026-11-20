package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Control requests: a request line written to stdin that Claude Code answers
// with a control_response carrying the same request_id. This file matches the
// two, so a caller can wait for the answer without holding up the reader.

type ctlReply struct {
	body json.RawMessage
	err  error
}

// controlResponse is a control_response line.
type controlResponse struct {
	Response struct {
		Subtype   string          `json:"subtype"` // "success" or "error"
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`
}

// call writes a control request and waits for its answer.
func (s *Session) call(ctx context.Context, p *proc, body map[string]any) (json.RawMessage, error) {
	id := fmt.Sprintf("harnessd-%d", s.reqSeq.Add(1))
	ch := make(chan ctlReply, 1)
	s.ctlMu.Lock()
	if s.ctl == nil {
		s.ctl = map[string]chan ctlReply{}
	}
	s.ctl[id] = ch
	s.ctlMu.Unlock()
	defer func() {
		s.ctlMu.Lock()
		delete(s.ctl, id)
		s.ctlMu.Unlock()
	}()

	line, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": body})
	if err != nil {
		return nil, err
	}
	if err := p.write(line); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.body, r.err
	case <-p.done:
		return nil, errors.New("claude process exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(controlTimeout):
		return nil, fmt.Errorf("claude did not answer %v in %s", body["subtype"], controlTimeout)
	}
}

const controlTimeout = 30 * time.Second

// controlReply hands a control_response to the call waiting for it. One nobody
// waits for (an interrupt's) is dropped.
func (s *Session) controlReply(raw []byte) {
	var r controlResponse
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	s.ctlMu.Lock()
	ch := s.ctl[r.Response.RequestID]
	s.ctlMu.Unlock()
	if ch == nil {
		return
	}
	reply := ctlReply{body: r.Response.Response}
	if r.Response.Subtype == "error" {
		reply.err = errors.New(firstNonEmpty(r.Response.Error, "control request failed"))
	}
	select {
	case ch <- reply:
	default:
	}
}
