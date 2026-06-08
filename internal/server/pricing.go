package server

import "strings"

// tokenRate is the per-token USD price for fresh input and output tokens. Both
// are dollars-per-single-token (i.e. the published $/MTok rate divided by 1e6).
type tokenRate struct {
	inputPerToken  float64
	outputPerToken float64
}

// modelPrice is one row of the published-rate table, expressed in dollars per
// million tokens for readability.
type modelPrice struct {
	// match is a lowercase substring identifying the model family. The first
	// row whose match is contained in the (lowercased) model id wins, so list
	// more specific families (e.g. "gpt-5.5") before broader ones ("gpt-5").
	match     string
	inPerM    float64
	outPerM   float64
}

// modelPrices is the per-family $/MTok table used to turn fresh work tokens
// into an estimated dollar cost for the Mission Control charts. The numbers are
// published list prices, not a billed invoice — the cost shown in the UI is an
// estimate of fresh-work cost (cache reads/creation excluded), consistent with
// the token figures it sits beside.
//
// Sources (captured 2026-06): Anthropic model pricing (Opus 4.6/4.7/4.8 $5/$25,
// Sonnet 4.6 $3/$15, Haiku 4.5 $1/$5) and OpenAI API pricing (GPT-5.5 $5/$30,
// GPT-5 $1.75/$14). Codex variants without a published rate fall back to the
// GPT-5 family rate. Order matters: most-specific family first.
var modelPrices = []modelPrice{
	// Anthropic
	{match: "opus", inPerM: 5.0, outPerM: 25.0},
	{match: "sonnet", inPerM: 3.0, outPerM: 15.0},
	{match: "haiku", inPerM: 1.0, outPerM: 5.0},
	// OpenAI / Codex — gpt-5.5 before the broader gpt-5 prefix.
	{match: "gpt-5.5", inPerM: 5.0, outPerM: 30.0},
	{match: "gpt-5", inPerM: 1.75, outPerM: 14.0},
}

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

// turnCostUSD prices one turn's fresh input + output tokens at the given rate.
func turnCostUSD(freshInput, freshOutput int, rate tokenRate) float64 {
	return float64(freshInput)*rate.inputPerToken + float64(freshOutput)*rate.outputPerToken
}
