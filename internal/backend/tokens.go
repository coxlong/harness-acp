package backend

import (
	"github.com/ironpark/acp-go/schema/meta"
	schema "github.com/ironpark/acp-go/schema/v2"
)

// TokenBreakdown is how one scope's tokens split between fresh input, cache
// and output. A usage_update has no field for it, so both backends report it
// as `_meta["_harness/tokens"]`.
//
// The counters are normalized, because the agents count differently: input
// excludes the cache (Codex's input includes it, Claude's does not) and output
// includes reasoning.
type TokenBreakdown struct {
	Input      uint64 `json:"input"`
	Cached     uint64 `json:"cached"`
	CacheWrite uint64 `json:"cacheWrite"`
	Output     uint64 `json:"output"`
}

// TokenMeta builds the usage metadata: how the context in use splits, and,
// when the backend knows it, the session's lifetime total. A nil breakdown is
// left out; no breakdown at all means no metadata.
func TokenMeta(context, lifetime *TokenBreakdown) schema.Meta {
	doc := map[string]any{}
	if context != nil {
		doc["context"] = *context
	}
	if lifetime != nil {
		doc["lifetime"] = *lifetime
	}
	if len(doc) == 0 {
		return nil
	}
	m, err := meta.Of(map[string]any{"_harness/tokens": doc})
	if err != nil {
		return nil
	}
	return m
}

// FreshInput is the prompt's tokens that were neither read from nor written to
// the cache. Codex reports input as the whole prompt, so its cache counters
// have to come off; an inconsistent report cannot go below zero.
func FreshInput(input, cached, write uint64) uint64 {
	if cached+write >= input {
		return 0
	}
	return input - cached - write
}
