// Package rws implements a resumable, reliable WebSocket transport for ACP.
//
// A logical connection (identified by an opaque server-issued ID) outlives the
// underlying physical WebSocket up to a configurable disconnect TTL (default
// 24h) and unacknowledged buffer budget. Both peers sequence every application
// message and piggyback cumulative acknowledgments so reconnects replay only
// unacknowledged frames in order, transparently to the ACP layer above.
//
// Wire framing (WebSocket text frames):
//   - Control frames: single-line JSON header {"t":"..."}
//   - Data frames:    {"t":"msg","s":<seq>,"a":<ack>}\n<raw ACP JSON>
//
// Separating the control header with a newline avoids parsing or re-encoding
// the ACP payload (which may carry multi-megabyte base64 attachments).
package rws

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
)

const (
	typeHello   = "hello"
	typeWelcome = "welcome"
	typeExpired = "expired"
	typeMsg     = "msg"
	typeAck     = "ack"
	typePing    = "ping"
	typePong    = "pong"
	typeBye     = "bye"
)

type header struct {
	Type    string `json:"t"`
	ID      string `json:"id,omitempty"`
	Seq     uint64 `json:"s,omitempty"`
	Ack     uint64 `json:"a,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	TTL     int64  `json:"ttl,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Nonce   uint64 `json:"n,omitempty"`
}

func encodeControl(h header) []byte {
	raw, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	return raw
}

func encodeMsg(seq, ack uint64, payload jsontext.Value) []byte {
	buf := make([]byte, 0, 48+len(payload))
	buf = fmt.Appendf(buf, `{"t":"msg","s":%d,"a":%d}`+"\n", seq, ack)
	return append(buf, payload...)
}

func decodeFrame(raw []byte) (header, jsontext.Value, error) {
	head := raw
	var payload jsontext.Value
	if nl := bytes.IndexByte(raw, '\n'); nl >= 0 {
		head = raw[:nl]
		payload = jsontext.Value(raw[nl+1:]).Clone()
	}
	var h header
	if err := json.Unmarshal(head, &h); err != nil {
		return header{}, nil, fmt.Errorf("rws: invalid header: %w", err)
	}
	if h.Type == "" {
		return header{}, nil, errors.New("rws: missing frame type")
	}
	return h, payload, nil
}
