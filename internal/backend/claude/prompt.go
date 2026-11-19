package claude

import (
	"encoding/json"
	"fmt"

	"github.com/ironpark/acp-go/acp2"
)

// userEnvelope is the stream-json user message written to claude's stdin.
// `origin: human` matters: Claude Code treats a message without it as
// unattributed and fails closed at its trust gates (claude-agent-acp stamps
// it for the same reason).
type userEnvelope struct {
	Type            string      `json:"type"`
	Message         userPayload `json:"message"`
	SessionID       string      `json:"session_id"`
	ParentToolUseID *string     `json:"parent_tool_use_id"`
	UUID            string      `json:"uuid"`
	Origin          origin      `json:"origin"`
}

type userPayload struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type origin struct {
	Kind string `json:"kind"`
}

// buildUserMessage converts an ACP prompt to the line claude reads.
//
// Like claude-agent-acp it takes text, image, resource_link and resource
// blocks. Any other block fails the request: silently dropping content the
// client sent would answer a different question than the one asked.
func buildUserMessage(sessionID, msgUUID string, blocks []acp2.ContentBlock) ([]byte, error) {
	if len(blocks) == 0 {
		return nil, fmt.Errorf("invalid params: prompt must hold at least one content block")
	}
	var content, context []any
	for _, b := range blocks {
		// Marshal to a generic map so this does not depend on the shape of the
		// generated union types.
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("invalid params: content block: %w", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("invalid params: content block: %w", err)
		}
		str := func(k string) string { s, _ := m[k].(string); return s }

		switch typ := str("type"); typ {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": str("text")})
		case "resource_link":
			content = append(content, map[string]any{"type": "text", "text": uriLink(str("uri"))})
		case "image":
			switch {
			case str("data") != "":
				content = append(content, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type": "base64", "data": str("data"), "media_type": str("mimeType"),
					},
				})
			case hasHTTPPrefix(str("uri")):
				content = append(content, map[string]any{
					"type": "image", "source": map[string]any{"type": "url", "url": str("uri")},
				})
			default:
				return nil, fmt.Errorf("invalid params: image block needs data or an http(s) uri")
			}
		case "resource":
			res, _ := m["resource"].(map[string]any)
			uri, _ := res["uri"].(string)
			text, ok := res["text"].(string)
			if !ok {
				return nil, fmt.Errorf("invalid params: embedded blob resource %q is not supported", uri)
			}
			content = append(content, map[string]any{"type": "text", "text": uriLink(uri)})
			context = append(context, map[string]any{
				"type": "text",
				"text": fmt.Sprintf("\n<context ref=%q>\n%s\n</context>", uri, text),
			})
		default:
			return nil, fmt.Errorf("invalid params: content blocks of type %q are not supported", typ)
		}
	}
	content = append(content, context...)

	return json.Marshal(userEnvelope{
		Type:      "user",
		Message:   userPayload{Role: "user", Content: content},
		SessionID: sessionID,
		UUID:      msgUUID,
		Origin:    origin{Kind: "human"},
	})
}

// uriLink renders file:// URIs as "[@name](uri)", as claude-agent-acp does.
func uriLink(uri string) string {
	const prefix = "file://"
	if len(uri) > len(prefix) && uri[:len(prefix)] == prefix {
		path := uri[len(prefix):]
		name := path
		for i := len(path) - 1; i >= 0; i-- {
			if path[i] == '/' {
				name = path[i+1:]
				break
			}
		}
		if name == "" {
			name = path
		}
		return "[@" + name + "](" + uri + ")"
	}
	return uri
}

func hasHTTPPrefix(s string) bool {
	return len(s) >= 4 && s[:4] == "http"
}
