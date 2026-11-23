package codex

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	schema "github.com/ironpark/acp-go/schema/v2"
)

const otherAnswer = "None of the above"

type inputQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsOther  bool   `json:"isOther"`
	IsSecret bool   `json:"isSecret"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

type answerField struct {
	id      string
	options []string
	note    string
}

type pendingInput struct {
	nativeID json.RawMessage
	fields   []answerField
	cancel   context.CancelFunc
	timer    *time.Timer
	blocking bool
}

func (s *Session) requestUserInput(m message) {
	var input struct {
		ItemID           string          `json:"itemId"`
		Questions        []inputQuestion `json:"questions"`
		IsBlocking       *bool           `json:"isBlocking"`
		AutoResolutionMS *int64          `json:"autoResolutionMs"`
	}
	if json.Unmarshal(m.Params, &input) != nil {
		s.answerInput(m.ID, nil)
		return
	}
	form, fields := inputForm(input.Questions)
	if len(fields) == 0 {
		s.answerInput(m.ID, nil)
		return
	}
	formReq := schema.CreateElicitationRequestFormSession{
		SessionID: schema.SessionID(s.id), RequestedSchema: form, Message: "Codex needs your input to continue.",
	}
	if input.ItemID != "" {
		formReq.ToolCallID = new(schema.ToolCallID(input.ItemID))
	}
	req, err := schema.NewCreateElicitationRequest(formReq)
	if err != nil {
		s.answerInput(m.ID, nil)
		return
	}

	key := string(m.ID)
	if _, exists := s.inputs[key]; exists {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := pendingInput{
		nativeID: m.ID, fields: fields, cancel: cancel,
		blocking: input.IsBlocking == nil || *input.IsBlocking,
	}
	reply := func(response *acp2.CreateElicitationResponse) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.inputs[key]; ok {
			wasBlocked := s.waitingForUser()
			s.removeInput(key, current)
			s.answerInput(current.nativeID, decodeInputAnswers(current.fields, response))
			s.syncUserAction(wasBlocked)
		}
	}
	if input.AutoResolutionMS != nil {
		wait := time.Duration(min(max(*input.AutoResolutionMS, 0), int64((1<<63-1)/time.Millisecond))) * time.Millisecond
		p.timer = time.AfterFunc(wait, func() { reply(nil) })
	}
	wasBlocked := s.waitingForUser()
	s.inputs[key] = p
	s.syncUserAction(wasBlocked)
	s.emit(backend.Event{Kind: backend.EventElicitation, Elicitation: &backend.Elicitation{
		Context: ctx, Request: req, Reply: reply,
	}})
}

func inputForm(questions []inputQuestion) (schema.ElicitationSchema, []answerField) {
	form := schema.ElicitationSchema{
		Type: schema.ElicitationSchemaTypeObject, Properties: make(map[string]schema.ElicitationPropertySchema),
	}
	used := make(map[string]bool)
	for _, question := range questions {
		// ACP form elicitation must not expose secrets. Codex's boolean is
		// the runtime's signal that it expects sensitive input.
		if question.IsSecret || question.ID == "" || used[question.ID] {
			return form, nil
		}
		used[question.ID] = true
	}
	fields := make([]answerField, 0, len(questions))
	for _, q := range questions {
		title := q.Question
		if title == "" {
			title = q.ID
		}
		property := schema.ElicitationPropertySchemaString{Title: &title}
		if q.Header != "" && q.Header != title {
			property.Description = &q.Header
		}
		field := answerField{id: q.ID}
		for _, option := range q.Options {
			if option.Label == "" || slices.Contains(field.options, option.Label) {
				continue
			}
			enum := schema.EnumOption{Const: option.Label, Title: option.Label}
			if option.Description != "" {
				enum.Description = &option.Description
			}
			field.options = append(field.options, option.Label)
			property.OneOf = append(property.OneOf, enum)
		}
		if q.IsOther && len(field.options) > 0 {
			if !slices.Contains(field.options, otherAnswer) {
				field.options = append(field.options, otherAnswer)
				property.OneOf = append(property.OneOf, schema.EnumOption{
					Const: otherAnswer, Title: otherAnswer, Description: new("Enter your own answer below."),
				})
			}
			field.note = q.ID + "_note"
			for used[field.note] {
				field.note += "_"
			}
			used[field.note] = true
			form.Properties[field.note] = schema.NewElicitationPropertySchema(schema.ElicitationPropertySchemaString{
				Title: new("Additional answer or note"),
			})
		}
		form.Properties[q.ID] = schema.NewElicitationPropertySchema(property)
		form.Required = append(form.Required, q.ID)
		fields = append(fields, field)
	}
	return form, fields
}

type inputAnswer struct {
	Answers []string `json:"answers"`
}

func decodeInputAnswers(fields []answerField, response *acp2.CreateElicitationResponse) map[string]inputAnswer {
	if response == nil {
		return nil
	}
	accepted, ok := response.Variant().(schema.CreateElicitationResponseAccept)
	if !ok {
		return nil
	}
	answers := make(map[string]inputAnswer)
	text := func(id string) string {
		value, _ := accepted.Content[id].As[string]()
		return strings.TrimSpace(value)
	}
	for _, field := range fields {
		value := text(field.id)
		if value == "" || (len(field.options) > 0 && !slices.Contains(field.options, value)) {
			continue
		}
		values := []string{value}
		if field.note != "" {
			if note := text(field.note); note != "" {
				values = append(values, "user_note: "+note)
			} else if value == otherAnswer {
				continue
			}
		}
		answers[field.id] = inputAnswer{values}
	}
	return answers
}

func (s *Session) answerInput(id json.RawMessage, answers map[string]inputAnswer) {
	if answers == nil {
		answers = map[string]inputAnswer{}
	}
	s.p.respond(id, object{"answers": answers}, nil)
}

func (s *Session) removeInput(key string, p pendingInput) {
	delete(s.inputs, key)
	p.cancel()
	if p.timer != nil {
		p.timer.Stop()
	}
}

func (s *Session) clearInputs() {
	for key, input := range s.inputs {
		s.removeInput(key, input)
	}
}

func (s *Session) waitingForUser() bool {
	if len(s.pending) > 0 {
		return true
	}
	for _, input := range s.inputs {
		if input.blocking {
			return true
		}
	}
	return false
}

func (s *Session) syncUserAction(wasBlocked bool) {
	blocked := s.waitingForUser()
	if s.turnID == "" || blocked == wasBlocked {
		return
	}
	if blocked {
		s.state(backend.TurnRequiresAction, "", nil)
	} else {
		s.state(backend.TurnRunning, "", nil)
	}
}
