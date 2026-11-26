package session

import (
	"encoding/base64"
	"sort"
	"unicode/utf8"

	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/acp2"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

type outputKind uint8

const (
	messageOutput outputKind = iota
	thoughtOutput
	terminalOutput
)

type outputKey struct {
	session string
	id      string
	kind    outputKind
}

type outputBuffer struct {
	first uint64
	last  uint64
	bytes []byte
}

// liveOutput captures in-flight streaming output that has not yet been
// persisted by Codex, so a cold replayFrom:start on an active session does not
// miss the current partial message, thought, or terminal chunk.
type liveOutput struct {
	seq     uint64
	streams map[outputKey]*outputBuffer
}

func newLiveOutput() *liveOutput {
	return &liveOutput{streams: make(map[outputKey]*outputBuffer)}
}

func (out *liveOutput) observe(ev backend.Event) {
	if out == nil || ev.Kind != backend.EventUpdate {
		return
	}
	out.seq++
	seq := out.seq
	key := outputKey{session: ev.Session}
	switch u := ev.Update.Variant().(type) {
	case schema.SessionUpdateAgentMessageChunk:
		key.id = string(u.MessageID)
		if text, ok := u.Content.Variant().(schema.ContentBlockText); ok {
			out.append(key, seq, []byte(text.Text))
		}
	case schema.SessionUpdateAgentThoughtChunk:
		key.id, key.kind = string(u.MessageID), thoughtOutput
		if text, ok := u.Content.Variant().(schema.ContentBlockText); ok {
			out.append(key, seq, []byte(text.Text))
		}
	case schema.SessionUpdateAgentMessage:
		if !u.Content.IsZero() {
			key.id = string(u.MessageID)
			delete(out.streams, key)
		}
	case schema.SessionUpdateAgentThought:
		if !u.Content.IsZero() {
			key.id, key.kind = string(u.MessageID), thoughtOutput
			delete(out.streams, key)
		}
	case schema.SessionUpdateTerminalOutputChunk:
		key.id, key.kind = string(u.TerminalID), terminalOutput
		if data, err := base64.StdEncoding.DecodeString(u.Data); err == nil {
			out.append(key, seq, data)
		}
	case schema.SessionUpdateTerminalUpdate:
		if u.Output.IsZero() {
			return
		}
		key.id, key.kind = string(u.TerminalID), terminalOutput
		delete(out.streams, key)
		if output, ok := u.Output.Get(); ok {
			if data, err := base64.StdEncoding.DecodeString(output.Data); err == nil {
				out.append(key, seq, data)
			}
		}
	}
}

func (out *liveOutput) append(key outputKey, sequence uint64, data []byte) {
	if len(data) == 0 {
		return
	}
	const maxStreams = 32
	const maxStreamBytes = 1 << 20
	buffer := out.streams[key]
	if buffer == nil {
		if len(out.streams) >= maxStreams {
			var oldest outputKey
			oldestSeq := sequence
			for k, b := range out.streams {
				if b.last < oldestSeq {
					oldest, oldestSeq = k, b.last
				}
			}
			delete(out.streams, oldest)
		}
		buffer = &outputBuffer{first: sequence}
		out.streams[key] = buffer
	}
	buffer.last = sequence
	if len(data) >= maxStreamBytes {
		data = data[len(data)-maxStreamBytes:]
		buffer.bytes = buffer.bytes[:0]
	}
	if over := len(buffer.bytes) + len(data) - maxStreamBytes; over > 0 {
		copy(buffer.bytes, buffer.bytes[over:])
		buffer.bytes = buffer.bytes[:len(buffer.bytes)-over]
	}
	buffer.bytes = append(buffer.bytes, data...)
}

func (out *liveOutput) snapshot() []backend.HistoryUpdate {
	if out == nil || len(out.streams) == 0 {
		return nil
	}
	keys := make([]outputKey, 0, len(out.streams))
	for key := range out.streams {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return out.streams[keys[i]].first < out.streams[keys[j]].first })
	updates := make([]backend.HistoryUpdate, 0, len(keys))
	for _, key := range keys {
		data := out.streams[key].bytes
		var snapshot acp2.SessionUpdate
		if key.kind == terminalOutput {
			snapshot = schema.NewSessionUpdate(schema.SessionUpdateTerminalUpdate{
				TerminalID: schema.TerminalID(key.id),
				Output:     optional.Of(schema.TerminalOutput{Data: base64.StdEncoding.EncodeToString(data)}),
			})
		} else {
			// Clipping a large text stream can start inside a UTF-8 rune.
			for len(data) > 0 && !utf8.RuneStart(data[0]) {
				data = data[1:]
			}
			content := optional.Of([]acp2.ContentBlock{acp2.TextBlock(string(data))})
			if key.kind == thoughtOutput {
				snapshot = schema.NewSessionUpdate(schema.SessionUpdateAgentThought{MessageID: acp2.MessageID(key.id), Content: content})
			} else {
				snapshot = schema.NewSessionUpdate(schema.SessionUpdateAgentMessage{MessageID: acp2.MessageID(key.id), Content: content})
			}
		}
		updates = append(updates, backend.HistoryUpdate{Session: key.session, Update: snapshot})
	}
	return updates
}
