package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coxlong/harness-acp/internal/backend"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// tokenCount is one `token_count` event of a rollout, with the snake_case keys
// Codex writes.
func tokenCount(last, total string, window uint64) string {
	return fmt.Sprintf(
		`{"payload":{"type":"token_count","info":{"total_token_usage":%s,"last_token_usage":%s,"model_context_window":%d}}}`,
		total, last, window,
	)
}

func counters(input, cached, output, total uint64) string {
	return fmt.Sprintf(
		`{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}`,
		input, cached, output, total,
	)
}

func writeRollout(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadRolloutUsageTakesTheLastReport(t *testing.T) {
	body := strings.Join([]string{
		`{"payload":{"type":"session_meta","info":null}}`,
		tokenCount(counters(100, 50, 10, 160), counters(100, 50, 10, 160), 1000),
		tokenCount(counters(200, 150, 20, 370), counters(300, 200, 30, 530), 2000),
		// A line still being written is not a report.
		`{"payload":{"type":"token_count","info":{"last_token_usa`,
	}, "\n")
	path := writeRollout(t, body+"\n")

	usage, err := readRolloutUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Last.TotalTokens != 370 || usage.Last.InputTokens != 200 || usage.ModelContextWindow != 2000 {
		t.Fatalf("expected the last complete report, got %+v", usage)
	}
	if usage.Total.TotalTokens != 530 {
		t.Fatalf("expected the thread's counters, got %+v", usage.Total)
	}
}

func TestReadRolloutUsageSkipsReportsWithoutCounters(t *testing.T) {
	body := strings.Join([]string{
		tokenCount(counters(80, 40, 8, 128), counters(80, 40, 8, 128), 900),
		// A report that carries rate limits only.
		`{"payload":{"type":"token_count","info":null}}`,
		tokenCount(`{}`, `{}`, 900),
	}, "\n")
	path := writeRollout(t, body+"\n")

	usage, err := readRolloutUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Last.TotalTokens != 128 {
		t.Fatalf("expected the only report with counters, got %+v", usage)
	}
}

func TestReadRolloutUsageWithoutReports(t *testing.T) {
	path := writeRollout(t, `{"payload":{"type":"session_meta"}}`+"\n")
	if _, err := readRolloutUsage(path); err != errNoUsage {
		t.Fatalf("expected errNoUsage, got %v", err)
	}
}

func TestRestoredUsageRequiresAReadablePath(t *testing.T) {
	b := New(t.Context(), "codex", nil)
	if _, err := b.restoredUsage("thread-4", ""); err != errNoRollout {
		t.Fatalf("expected errNoRollout, got %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.jsonl")
	if _, err := b.restoredUsage("thread-4", missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing rollout error, got %v", err)
	}
}

func TestRestoredUsageFollowsTheRollout(t *testing.T) {
	path := writeRollout(t,
		tokenCount(counters(10, 5, 1, 16), counters(10, 5, 1, 16), 1000)+"\n")
	b := New(t.Context(), "codex", nil)

	first, err := b.restoredUsage("thread-4", path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Last.TotalTokens != 16 {
		t.Fatalf("unexpected first read: %+v", first)
	}

	// The thread ran again: the file grew, so the cache must not answer.
	body := tokenCount(counters(10, 5, 1, 16), counters(10, 5, 1, 16), 1000) + "\n" +
		tokenCount(counters(40, 30, 4, 74), counters(50, 35, 5, 90), 2000) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := b.restoredUsage("thread-4", path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Last.TotalTokens != 74 || second.ModelContextWindow != 2000 {
		t.Fatalf("the cache answered with a stale report: %+v", second)
	}
}

func TestUsageEventCarriesTheBreakdown(t *testing.T) {
	event := usageEvent(tokenUsage{
		Last: tokenCounters{
			InputTokens: 133235, CachedInputTokens: 132992,
			OutputTokens: 906, TotalTokens: 134141,
		},
		Total: tokenCounters{
			InputTokens: 2231905, CachedInputTokens: 2068992,
			OutputTokens: 5436, TotalTokens: 2237341,
		},
		ModelContextWindow: 475000,
	})
	update, ok := event.Update.Variant().(schema.SessionUpdateUsageUpdate)
	if !ok {
		t.Fatalf("expected a usage update, got %#v", event.Update.Variant())
	}
	if update.Used != 134141 || update.Size != 475000 {
		t.Fatalf("unexpected usage: %+v", update)
	}

	var doc struct {
		Context  backend.TokenBreakdown `json:"context"`
		Lifetime backend.TokenBreakdown `json:"lifetime"`
	}
	if err := json.Unmarshal([]byte(update.Meta["_harness/tokens"]), &doc); err != nil {
		t.Fatal(err)
	}
	// Codex's input is the whole prompt, so the cache comes off it.
	if got := doc.Context; got.Input != 243 || got.Cached != 132992 || got.Output != 906 {
		t.Fatalf("unexpected context breakdown: %+v", got)
	}
	// The split adds up to the context in use, which is what makes it a
	// breakdown of `used` rather than an unrelated figure.
	if got := doc.Context; got.Input+got.Cached+got.CacheWrite+got.Output != update.Used {
		t.Fatalf("breakdown %+v does not add up to used %d", got, update.Used)
	}
	if got := doc.Lifetime; got.Input != 162913 || got.Cached != 2068992 || got.Output != 5436 {
		t.Fatalf("unexpected lifetime breakdown: %+v", got)
	}
}

func TestUsageEventOmitsUnavailableContextBreakdown(t *testing.T) {
	event := usageEvent(tokenUsage{
		Last:               tokenCounters{TotalTokens: 23214},
		Total:              tokenCounters{InputTokens: 25000, CachedInputTokens: 20000, OutputTokens: 1000, TotalTokens: 26000},
		ModelContextWindow: 475000,
	})
	update, ok := event.Update.Variant().(schema.SessionUpdateUsageUpdate)
	if !ok || update.Used != 23214 || update.Size != 475000 {
		t.Fatalf("unexpected usage: %#v", event.Update.Variant())
	}
	var doc map[string]backend.TokenBreakdown
	if err := json.Unmarshal([]byte(update.Meta["_harness/tokens"]), &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["context"]; present {
		t.Fatalf("context breakdown should be absent, got %+v", doc["context"])
	}
	if want := (backend.TokenBreakdown{Input: 5000, Cached: 20000, Output: 1000}); doc["lifetime"] != want {
		t.Fatalf("lifetime breakdown = %+v, want %+v", doc["lifetime"], want)
	}
}
