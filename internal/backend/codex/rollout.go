package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// Codex records a thread's usage in the rollout it writes next to it: one
// `token_count` event per model response, the last of which describes the
// context the thread would carry on. Nothing in the app-server protocol asks
// for that number, and the thread and its turns carry no counters, so a
// resumed session has no other way to learn what it left behind.
//
// The record format belongs to Codex. If it changes, the session can still
// resume and report live usage on its next turn.

var (
	errNoRollout = errors.New("Codex returned no rollout path")
	errNoUsage   = errors.New("the rollout records no usage")
)

// tailChunk is how much of the rollout is read at a time, from its end
// backwards. Usage sits in the last few lines, so a chunk or two is enough.
const tailChunk = 64 << 10

// rolloutTokenCounters is one side of a rollout's `token_count`, whose keys are
// snake_case where the app-server notification's are camelCase.
type rolloutTokenCounters struct {
	InputTokens           uint64 `json:"input_tokens"`
	CachedInputTokens     uint64 `json:"cached_input_tokens"`
	CacheWriteInputTokens uint64 `json:"cache_write_input_tokens"`
	OutputTokens          uint64 `json:"output_tokens"`
	TotalTokens           uint64 `json:"total_tokens"`
}

// rolloutLine is the part of a rollout line that reports usage.
type rolloutLine struct {
	Payload struct {
		Type string `json:"type"`
		Info *struct {
			Total              rolloutTokenCounters `json:"total_token_usage"`
			Last               rolloutTokenCounters `json:"last_token_usage"`
			ModelContextWindow uint64               `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

// rolloutUsage is the last usage a thread's rollout recorded, in the shape the
// app-server notification has.
func (r rolloutLine) usage() tokenUsage {
	info := r.Payload.Info
	return tokenUsage{
		Total: tokenCounters{
			InputTokens:           info.Total.InputTokens,
			CachedInputTokens:     info.Total.CachedInputTokens,
			CacheWriteInputTokens: info.Total.CacheWriteInputTokens,
			OutputTokens:          info.Total.OutputTokens,
			TotalTokens:           info.Total.TotalTokens,
		},
		Last: tokenCounters{
			InputTokens:           info.Last.InputTokens,
			CachedInputTokens:     info.Last.CachedInputTokens,
			CacheWriteInputTokens: info.Last.CacheWriteInputTokens,
			OutputTokens:          info.Last.OutputTokens,
			TotalTokens:           info.Last.TotalTokens,
		},
		ModelContextWindow: info.ModelContextWindow,
	}
}

// readRolloutUsage reads a rollout's last usage. It walks the file backwards so
// a long session costs a read of its tail, not of the whole file.
func readRolloutUsage(path string) (tokenUsage, error) {
	f, err := os.Open(path)
	if err != nil {
		return tokenUsage{}, err
	}
	defer func() { _ = f.Close() }()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return tokenUsage{}, err
	}
	var carry []byte // the line the previous chunk started in the middle of
	for end := size; end > 0; {
		start := end - tailChunk
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, end-start)
		if _, err := f.ReadAt(chunk, start); err != nil && !errors.Is(err, io.EOF) {
			return tokenUsage{}, err
		}
		lines := bytes.Split(append(chunk, carry...), []byte("\n"))
		carry = nil
		if start > 0 {
			// The chunk began inside this line: it needs the bytes before it.
			carry, lines = lines[0], lines[1:]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if usage, ok := parseRolloutLine(lines[i]); ok {
				return usage, nil
			}
		}
		end = start
	}
	return tokenUsage{}, errNoUsage
}

// parseRolloutLine reads one rollout line, reporting the usage it records. A
// line still being written, and one that reports only rate limits, are not it.
func parseRolloutLine(line []byte) (tokenUsage, bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return tokenUsage{}, false
	}
	var parsed rolloutLine
	if json.Unmarshal(line, &parsed) != nil {
		return tokenUsage{}, false
	}
	if parsed.Payload.Type != "token_count" || parsed.Payload.Info == nil {
		return tokenUsage{}, false
	}
	usage := parsed.usage()
	if usage.Last.TotalTokens == 0 || usage.ModelContextWindow == 0 {
		return tokenUsage{}, false
	}
	return usage, true
}

// rollout is the usage read from one thread's rollout, kept until the file it
// came from changes.
type rollout struct {
	path  string
	mtime time.Time
	size  int64
	usage tokenUsage
}

// restoredUsage reads the usage from the rollout path Codex supplied.
func (b *Backend) restoredUsage(threadID, path string) (tokenUsage, error) {
	if path == "" {
		return tokenUsage{}, errNoRollout
	}
	info, err := os.Stat(path)
	if err != nil {
		return tokenUsage{}, err
	}

	b.mu.Lock()
	cached := b.rollouts[threadID]
	b.mu.Unlock()
	if cached != nil && cached.path == path && cached.size == info.Size() && cached.mtime.Equal(info.ModTime()) {
		return cached.usage, nil
	}

	usage, err := readRolloutUsage(path)
	if err != nil {
		return tokenUsage{}, err
	}
	b.mu.Lock()
	if b.rollouts == nil {
		b.rollouts = make(map[string]*rollout)
	}
	b.rollouts[threadID] = &rollout{path: path, mtime: info.ModTime(), size: info.Size(), usage: usage}
	b.mu.Unlock()
	return usage, nil
}
