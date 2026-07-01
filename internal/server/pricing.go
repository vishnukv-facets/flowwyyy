package server

import "strings"

// tokenRate is the per-token USD price for fresh input and output tokens. Both
// are dollars-per-single-token (i.e. the published $/MTok rate divided by 1e6).
type tokenRate struct {
	inputPerToken  float64
	outputPerToken float64
}

type tokenCostSplit struct {
	Fresh         float64
	CacheRead     float64
	CacheCreation float64
}

func (s tokenCostSplit) total() float64 {
	return s.Fresh + s.CacheRead + s.CacheCreation
}

// modelPrice is one row of the published-rate table, expressed in dollars per
// million tokens for readability.
type modelPrice struct {
	// match is a lowercase substring identifying the model family. The first
	// row whose match is contained in the (lowercased) model id wins, so list
	// more specific families (e.g. "gpt-5.5") before broader ones ("gpt-5").
	match   string
	inPerM  float64
	outPerM float64
}

// modelPrices is the per-family $/MTok table used to turn billed tokens into an
// estimated dollar cost for the Mission Control charts. The numbers are
// published list prices, not a billed invoice — but unlike the token "work"
// figures it sits beside (which exclude caching), the dollar estimate counts
// the FULL bill: fresh input + output PLUS cache reads and cache creation
// priced at their cache multipliers (see billedCostSplitUSD). On a long agentic
// session cache reads dominate the bill, so excluding them understated cost by
// multiples — this table + billedCostSplitUSD is what makes the figure track Claude
// Code's own /cost.
//
// Sources (captured 2026-06): Anthropic model pricing (Fable 5 $10/$50, Opus
// 4.6/4.7/4.8 $5/$25, Sonnet 4.6 $3/$15, Haiku 4.5 $1/$5) and OpenAI API
// pricing (GPT-5.5 $5/$30, GPT-5.2-Codex $1.75/$14). Codex variants without a
// published rate fall back to the gpt-5 family rate. Order matters:
// most-specific family first ("fable" before nothing, "gpt-5.5" before "gpt-5").
var modelPrices = []modelPrice{
	// Anthropic
	{match: "fable", inPerM: 10.0, outPerM: 50.0},
	{match: "opus", inPerM: 5.0, outPerM: 25.0},
	{match: "sonnet", inPerM: 3.0, outPerM: 15.0},
	{match: "haiku", inPerM: 1.0, outPerM: 5.0},
	// OpenAI / Codex — gpt-5.5 before the broader gpt-5 prefix.
	{match: "gpt-5.5", inPerM: 5.0, outPerM: 30.0},
	{match: "gpt-5", inPerM: 1.75, outPerM: 14.0},
}

// Cache multipliers on the input rate. Cache reads bill at 0.1x the input rate
// (Anthropic prompt-cache reads and OpenAI's 90%-discounted cached input both
// land here); Anthropic cache WRITES bill at 1.25x for the 5-minute TTL and 2x
// for the 1-hour TTL. OpenAI has no cache-write surcharge, so Codex never hits
// the write multipliers (it reports no cache_creation tokens).
const (
	cacheReadMultiplier    = 0.1
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// modelTokenRate returns the per-token USD rate for a model id. Matching is by
// lowercased family substring (so "claude-opus-4-8[1m]", "claude-opus-4-8", and
// a dated opus snapshot all price as Opus). An empty or unrecognized model
// returns a zero rate — we under-count rather than fabricate a price, so an
// unpriced session shows tokens with no dollar figure instead of a made-up one.
func modelTokenRate(model string) tokenRate {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return tokenRate{}
	}
	for _, p := range modelPrices {
		if strings.Contains(m, p.match) {
			return tokenRate{inputPerToken: p.inPerM / 1_000_000, outputPerToken: p.outPerM / 1_000_000}
		}
	}
	return tokenRate{}
}

// turnCostUSD prices fresh input + output tokens at the given rate. Used by the
// Codex path, which prices the input/output portion of a running-total delta;
// the Codex cached-read portion is added separately via cacheReadCostUSD.
func turnCostUSD(freshInput, freshOutput int, rate tokenRate) float64 {
	return float64(freshInput)*rate.inputPerToken + float64(freshOutput)*rate.outputPerToken
}

// cacheReadCostUSD prices cache-hit tokens at 0.1x the input rate.
func cacheReadCostUSD(cacheReadTokens int, rate tokenRate) float64 {
	return float64(cacheReadTokens) * rate.inputPerToken * cacheReadMultiplier
}

func (u transcriptTokenUsage) cacheCreationCostUSD(rate tokenRate) float64 {
	in := rate.inputPerToken
	cost := float64(u.CacheCreation.Ephemeral5m) * in * cacheWrite5mMultiplier
	cost += float64(u.CacheCreation.Ephemeral1h) * in * cacheWrite1hMultiplier
	// Older transcripts report cache_creation_input_tokens without the 5m/1h
	// breakdown; price any unattributed remainder at the 5-minute (default-TTL)
	// rate so a missing breakdown under-prices rather than over-prices.
	if extra := u.CacheCreationInputTokens - (u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h); extra > 0 {
		cost += float64(extra) * in * cacheWrite5mMultiplier
	}
	return cost
}

func (u transcriptTokenUsage) billedCostSplitUSD(rate tokenRate) tokenCostSplit {
	return tokenCostSplit{
		Fresh:         turnCostUSD(u.freshInput(), u.freshOutput(), rate),
		CacheRead:     cacheReadCostUSD(u.cacheReadTokens(), rate),
		CacheCreation: u.cacheCreationCostUSD(rate),
	}
}
