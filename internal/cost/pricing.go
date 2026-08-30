package cost

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

//go:embed pricing_data.json
var bundledPricingJSON []byte

// ModelPrice is one model's per-token USD rates. Field tags match the LiteLLM
// model_prices table so the bundled snapshot (or an upstream refresh) parses
// directly; CacheCreation1h is our addition for Anthropic's 1-hour cache TTL.
type ModelPrice struct {
	Input           float64 `json:"input_cost_per_token"`
	Output          float64 `json:"output_cost_per_token"`
	CacheCreation   float64 `json:"cache_creation_input_token_cost"`    // 5-minute TTL (1.25x input)
	CacheCreation1h float64 `json:"cache_creation_input_token_cost_1h"` // 1-hour TTL (2x input)
	CacheRead       float64 `json:"cache_read_input_token_cost"`
}

// Usage is the token breakdown read from a transcript turn. The Ephemeral*
// fields capture Claude Code's split of cache-creation tokens by TTL; when a
// transcript omits the split, CacheCreation is priced wholly at the 5m rate
// (matching LiteLLM's single-rate behavior).
type Usage struct {
	InputTokens          int64
	OutputTokens         int64
	CacheReadInputTokens int64
	CacheCreationInput   int64 // total, used when the TTL split is absent
	Ephemeral5mTokens    int64
	Ephemeral1hTokens    int64
}

// PriceTable holds the resolved per-model rates plus the alias map that maps
// transcript model strings (often short aliases like "claude-opus-4-8" or
// dated variants) onto table keys.
type PriceTable struct {
	models map[string]ModelPrice
}

// modelAliases maps transcript model strings that don't match a table key
// directly onto one that does. Kept small on purpose — ResolvePrice also does
// suffix-stripping and prefix matching, so this only needs the genuinely
// irregular cases.
var modelAliases = map[string]string{
	"claude-3-5-haiku-20241022": "claude-3-5-haiku",
	"claude-3-5-haiku-latest":   "claude-3-5-haiku",
	// Common passthrough strings from Claude Code / Cursor hooks (cli#63).
	"claude-sonnet-4":       "claude-sonnet-4-6",
	"claude-sonnet-4-5":     "claude-sonnet-4-5",
	"claude-opus-4":         "claude-opus-4-6",
	"claude-opus-4-8":       "claude-opus-4-8",
	"claude-haiku-4-5":      "claude-haiku-4-5",
	"composer-1":            "cursor-composer-1",
	"claude-4.5-sonnet":     "claude-sonnet-4-5",
	"claude-4.5-opus":       "claude-opus-4-5",
	// Cursor Grok fast slugs include effort + speed suffixes; suffix-trim would
	// land on the cheaper standard rate without these aliases. Short grok-*
	// payloads (grok-4.6-high-fast) reach these via the cursor- prefix retry
	// in ResolvePrice.
	"cursor-grok-4.6-high-fast": "cursor-grok-4.6-fast",
	"cursor-grok-4.5-high-fast": "cursor-grok-4.5-fast",
}

// LoadBundledPriceTable parses the embedded snapshot only. Tests use this for
// deterministic rates; commands use LoadPriceTable so a user's refreshed cache
// (if any) is layered in.
func LoadBundledPriceTable() (*PriceTable, error) {
	return parsePriceTable(bundledPricingJSON)
}

// CachedPricingPath is where an opt-in `cost refresh-pricing` writes the
// fetched upstream table (~/.config/promptconduit/cost/pricing.json). It is
// read locally — no network — by LoadPriceTable.
func CachedPricingPath() string {
	return filepath.Join(StoreDir(), "pricing.json")
}

// LoadPriceTable returns the curated embedded rates with the user's refreshed
// cache (if present) layered in as a fallback for models the curated table
// doesn't cover. Curated entries always win — they carry knowledge the upstream
// table lacks (Anthropic's 1-hour cache rate, Cursor Composer rates). This read
// is purely local; the network fetch lives behind the explicit
// `cost refresh-pricing` command.
func LoadPriceTable() (*PriceTable, error) {
	base, err := parsePriceTable(bundledPricingJSON)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(CachedPricingPath())
	if err != nil {
		return base, nil // no cache yet — embedded only
	}
	cached, err := parsePriceTable(data)
	if err != nil {
		return base, nil // corrupt cache — ignore, fall back to embedded
	}
	for key, mp := range cached.models {
		if _, exists := base.models[key]; exists {
			continue // curated entry wins
		}
		if mp.Input == 0 && mp.Output == 0 {
			continue // skip free/non-chat entries (embeddings, etc.)
		}
		base.models[key] = mp
	}
	return base, nil
}

// ValidatePricingData parses raw pricing JSON and returns how many priced
// models it contains. Used by `cost refresh-pricing` to validate a fetched
// table before caching it.
func ValidatePricingData(data []byte) (int, error) {
	t, err := parsePriceTable(data)
	if err != nil {
		return 0, err
	}
	return len(t.models), nil
}

func parsePriceTable(data []byte) (*PriceTable, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	models := make(map[string]ModelPrice, len(raw))
	for key, val := range raw {
		if strings.HasPrefix(key, "_") {
			continue // skip "_comment" and other metadata keys
		}
		var mp ModelPrice
		if err := json.Unmarshal(val, &mp); err != nil {
			continue // tolerate a malformed entry rather than fail the whole table
		}
		models[key] = mp
	}
	return &PriceTable{models: models}, nil
}

// ResolvePrice looks up a model's rates. Resolution order: exact key, alias
// map, then progressively shorter dash-delimited prefixes (so a dated or
// suffixed variant like "claude-opus-4-8-20260101" still resolves to
// "claude-opus-4-8"). Cursor hook payloads often send the short Grok slug
// (grok-4.6, grok-4.6-high) instead of the table key (cursor-grok-4.6); when
// the first pass misses and the slug starts with "grok-", we retry as
// "cursor-"+slug so the same exact/alias/trim path applies. The bool is false
// when nothing matches — callers should flag the event ModelPriced=false and
// charge zero rather than guess.
func (t *PriceTable) ResolvePrice(model string) (ModelPrice, bool) {
	if model == "" {
		return ModelPrice{}, false
	}
	if mp, ok := t.lookup(model); ok {
		return mp, true
	}
	if strings.HasPrefix(model, "grok-") {
		return t.lookup("cursor-" + model)
	}
	return ModelPrice{}, false
}

func (t *PriceTable) lookup(model string) (ModelPrice, bool) {
	if mp, ok := t.models[model]; ok {
		return mp, true
	}
	if canonical, ok := modelAliases[model]; ok {
		if mp, ok := t.models[canonical]; ok {
			return mp, true
		}
	}
	// Strip trailing dash-delimited segments one at a time and retry, so dated
	// or speed-suffixed IDs collapse to their base model key.
	for trimmed := model; ; {
		idx := strings.LastIndex(trimmed, "-")
		if idx <= 0 {
			break
		}
		trimmed = trimmed[:idx]
		if mp, ok := t.models[trimmed]; ok {
			return mp, true
		}
	}
	return ModelPrice{}, false
}

// CostForUsage prices a usage block. It returns the dollar breakdown and the
// total cache-creation token count (5m + 1h) folded for the Tokens struct.
// When the 5m/1h split is present it prices each tier at its own rate;
// otherwise it prices the whole CacheCreationInput total at the 5m rate.
func CostForUsage(u Usage, mp ModelPrice) (Cost, int64) {
	var cacheWriteTokens, cacheWriteCost = cacheWriteCostFor(u, mp)

	c := Cost{
		Input:      float64(u.InputTokens) * mp.Input,
		Output:     float64(u.OutputTokens) * mp.Output,
		CacheRead:  float64(u.CacheReadInputTokens) * mp.CacheRead,
		CacheWrite: cacheWriteCost,
		Currency:   Currency,
	}
	c.Total = c.Input + c.Output + c.CacheRead + c.CacheWrite
	return c, cacheWriteTokens
}

// cacheWriteCostFor returns the total cache-creation token count and its cost,
// honoring the 5m/1h split when the transcript provides it.
func cacheWriteCostFor(u Usage, mp ModelPrice) (tokens int64, cost float64) {
	rate1h := mp.CacheCreation1h
	if rate1h == 0 {
		rate1h = mp.CacheCreation // fall back to the 5m rate if 1h isn't priced
	}
	if u.Ephemeral5mTokens > 0 || u.Ephemeral1hTokens > 0 {
		tokens = u.Ephemeral5mTokens + u.Ephemeral1hTokens
		cost = float64(u.Ephemeral5mTokens)*mp.CacheCreation + float64(u.Ephemeral1hTokens)*rate1h
		return tokens, cost
	}
	// No split available — price the lumped total at the 5m rate.
	return u.CacheCreationInput, float64(u.CacheCreationInput) * mp.CacheCreation
}
