package claude

import (
	"github.com/coxlong/harness-acp/internal/backend"
	"github.com/ironpark/acp-go/schema/optional"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// defaultContextWindow is the window reported until Claude Code says what it
// is (a result's modelUsage): 1M, what Claude Code itself assumes for a model
// it does not know.
const defaultContextWindow = 1_000_000

// usage is the token usage of an API message or stream event. The cache
// counters are null when a backend does not report them.
type usage struct {
	InputTokens         *uint64 `json:"input_tokens"`
	OutputTokens        *uint64 `json:"output_tokens"`
	CacheReadTokens     *uint64 `json:"cache_read_input_tokens"`
	CacheCreationTokens *uint64 `json:"cache_creation_input_tokens"`
}

type usageSnapshot struct{ in, out, read, write uint64 }

func (u usageSnapshot) total() uint64 { return u.in + u.out + u.read + u.write }

func snapshot(u *usage) usageSnapshot {
	if u == nil {
		return usageSnapshot{}
	}
	n := func(p *uint64) uint64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return usageSnapshot{n(u.InputTokens), n(u.OutputTokens), n(u.CacheReadTokens), n(u.CacheCreationTokens)}
}

// usageTracker follows the context window figures of the main conversation, as
// claude-agent-acp's reports.ts does: the used tokens are the latest request's
// input, cache and output counters, whose sum is the next request's context.
type usageTracker struct {
	last   usageSnapshot
	total  uint64
	seen   bool // total is set
	window uint64
	model  string
}

func (u *usageTracker) size() uint64 {
	if u.window == 0 {
		return defaultContextWindow
	}
	return u.window
}

func (u *usageTracker) event(used uint64, cost *float64) backend.Event {
	v := schema.SessionUpdateUsageUpdate{Used: used, Size: u.size()}
	if cost != nil {
		v.Cost = optional.Of(schema.Cost{Amount: *cost, Currency: "USD"})
	}
	return update(v)
}

// messageStart reads a stream's message_start: the request's input counters.
func (u *usageTracker) messageStart(m message) (backend.Event, bool) {
	if m.Model == "<synthetic>" {
		return backend.Event{}, false
	}
	if m.Model != "" {
		u.model = m.Model
	}
	u.last = snapshot(m.Usage)
	return u.changed()
}

// messageDelta reads a stream's message_delta, whose counters are cumulative; a
// counter it leaves out keeps its value.
func (u *usageTracker) messageDelta(d *usage) (backend.Event, bool) {
	if d == nil {
		return backend.Event{}, false
	}
	if d.InputTokens != nil {
		u.last.in = *d.InputTokens
	}
	if d.OutputTokens != nil {
		u.last.out = *d.OutputTokens
	}
	if d.CacheReadTokens != nil {
		u.last.read = *d.CacheReadTokens
	}
	if d.CacheCreationTokens != nil {
		u.last.write = *d.CacheCreationTokens
	}
	return u.changed()
}

func (u *usageTracker) changed() (backend.Event, bool) {
	used := u.last.total()
	if u.seen && used == u.total {
		return backend.Event{}, false
	}
	u.total, u.seen = used, true
	return u.event(used, nil), true
}

// assistant records a finished message's usage, without a report: its stream
// reported it already.
func (u *usageTracker) assistant(m message) {
	if m.Model == "<synthetic>" {
		return
	}
	u.last = snapshot(m.Usage)
	u.total, u.seen = u.last.total(), true
	if m.Model != "" {
		u.model = m.Model
	}
}

// result learns the context window from the result's modelUsage and reports
// the turn's usage with the session's cost so far.
func (u *usageTracker) result(r *resultMsg) (backend.Event, bool) {
	if key := bestModelKey(r.ModelUsage, u.model); key != "" {
		if w := r.ModelUsage[key].ContextWindow; w > 0 {
			u.window = w
		}
	}
	if !u.seen {
		return backend.Event{}, false
	}
	return u.event(u.total, r.TotalCostUSD), true
}

// compacted sets the context that a compaction kept.
func (u *usageTracker) compacted(post uint64) backend.Event {
	u.last = usageSnapshot{}
	u.total, u.seen = post, true
	return u.event(post, nil)
}

// bestModelKey picks the modelUsage entry of the model in use: the key sharing
// the longest prefix with it ("claude-sonnet-5[1m]" for "claude-sonnet-5").
func bestModelKey[V any](m map[string]V, model string) string {
	best, bestLen := "", 0
	for k := range m {
		n := 0
		for n < len(k) && n < len(model) && k[n] == model[n] {
			n++
		}
		if n > bestLen || (n == bestLen && n > 0 && k < best) {
			best, bestLen = k, n
		}
	}
	return best
}
