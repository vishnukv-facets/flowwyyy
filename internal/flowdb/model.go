package flowdb

import (
	"fmt"
	"os"
	"strings"
)

// Session model selection.
//
// A task may carry an explicit model (the literal value passed to
// `claude --model` / `codex --model`). When it doesn't, flow picks one from a
// small tier ladder: a baseline tier (configurable, default medium) that is
// upshifted one rung for high-priority work (so important autonomous tasks get
// the stronger model) and downshifted one rung when the task's brief is
// descriptive enough that a cheaper model can do the job. The DB stores only
// the explicit choice; resolution happens at launch time in `flow do` so a
// brief edit or priority change moves the auto-pick on the next bootstrap
// without a migration.

// Model tiers, smallest to largest.
const (
	ModelTierSmall  = "small"
	ModelTierMedium = "medium"
	ModelTierLarge  = "large"
)

// DefaultModelTier is the baseline tier used when FLOW_MODEL_TIER is unset.
// It is deliberately not the largest tier — the point of this feature is to
// stop defaulting every task to the most capable (and most expensive) model.
const DefaultModelTier = ModelTierMedium

// NormalizeModel trims an explicit per-task model value. Empty stays empty
// ("no explicit choice — let flow decide"). Validation is intentionally light:
// flow recognizes known tier aliases (opus/sonnet/haiku, gpt-5.4*) for the
// menu and downshift ranking, but passes any other value straight through to
// the provider CLI, since Claude and Codex ship new model ids frequently.
func NormalizeModel(model string) string {
	return strings.TrimSpace(model)
}

// NormalizeModelTier canonicalizes a tier name. Empty maps to the default.
func NormalizeModelTier(tier string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(tier)) {
	case "":
		return DefaultModelTier, nil
	case ModelTierSmall:
		return ModelTierSmall, nil
	case ModelTierMedium:
		return ModelTierMedium, nil
	case ModelTierLarge:
		return ModelTierLarge, nil
	default:
		return "", fmt.Errorf("model tier must be small|medium|large, got %q", tier)
	}
}

// DownshiftTier returns the tier one rung below the given tier. small is the
// floor (it cannot downshift further).
func DownshiftTier(tier string) string {
	switch tier {
	case ModelTierLarge:
		return ModelTierMedium
	case ModelTierMedium:
		return ModelTierSmall
	default:
		return ModelTierSmall
	}
}

// UpshiftTier returns the tier one rung above the given tier. large is the
// ceiling (it cannot upshift further).
func UpshiftTier(tier string) string {
	switch tier {
	case ModelTierSmall:
		return ModelTierMedium
	case ModelTierMedium:
		return ModelTierLarge
	default:
		return ModelTierLarge
	}
}

// ModelForTier maps a (provider, tier) pair to the concrete model value passed
// to the agent CLI. Claude uses the friendly aliases that `claude --model`
// resolves to the latest of each tier (so they never go stale); Codex uses the
// gpt-5.4 family ids. Unknown providers fall back to the Claude ladder, and an
// unknown tier falls back to medium.
func ModelForTier(provider, tier string) string {
	if strings.EqualFold(strings.TrimSpace(provider), "codex") {
		switch tier {
		case ModelTierSmall:
			return "gpt-5.4-mini"
		case ModelTierLarge:
			return "gpt-5.5"
		default:
			return "gpt-5.4"
		}
	}
	switch tier {
	case ModelTierSmall:
		return "haiku"
	case ModelTierLarge:
		return "opus"
	default:
		return "sonnet"
	}
}

// ModelTierFromEnv reads the baseline tier from FLOW_MODEL_TIER, falling back
// to DefaultModelTier when unset or invalid (a bad value should never crash a
// launch — it degrades to the sensible default).
func ModelTierFromEnv() string {
	tier, err := NormalizeModelTier(os.Getenv("FLOW_MODEL_TIER"))
	if err != nil {
		return DefaultModelTier
	}
	return tier
}

// AutoDownshiftEnabled reports whether descriptive briefs should downshift to a
// smaller model. Default is enabled; FLOW_MODEL_AUTODOWNSHIFT set to a falsey
// value (off/0/false/no) disables it.
func AutoDownshiftEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("FLOW_MODEL_AUTODOWNSHIFT"))) {
	case "off", "0", "false", "no":
		return false
	default:
		return true
	}
}

// Descriptiveness heuristic thresholds. A brief is "descriptive enough" for a
// smaller model when it has no deferred sections, enough prose to pin down the
// work, and a couple of concrete acceptance criteria. These are deliberately
// simple and deterministic so the downshift decision is explainable and
// testable; FLOW_MODEL_AUTODOWNSHIFT=off turns the whole thing off, and an
// explicit per-task model always overrides it.
const (
	descriptiveMinWords    = 80
	descriptiveMinDoneWhen = 2
)

// BriefIsDescriptive reports whether a task brief is specific enough that a
// smaller model can handle it. The signals: no `*Deferred*` placeholder
// sections, at least descriptiveMinWords words, and at least
// descriptiveMinDoneWhen concrete "Done when" bullets.
func BriefIsDescriptive(brief string) bool {
	if strings.Contains(brief, "*Deferred") {
		return false
	}
	if len(strings.Fields(brief)) < descriptiveMinWords {
		return false
	}
	return countDoneWhenBullets(brief) >= descriptiveMinDoneWhen
}

// countDoneWhenBullets counts the concrete bullet lines under the "## Done when"
// heading, stopping at the next "## " heading. Placeholder/italic lines (e.g.
// "*Deferred*") and empty bullets are not counted.
func countDoneWhenBullets(brief string) int {
	lines := strings.Split(brief, "\n")
	inSection := false
	count := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inSection = strings.EqualFold(trimmed, "## Done when")
			continue
		}
		if !inSection {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "- "); ok {
			body := strings.TrimSpace(rest)
			if body != "" && !strings.HasPrefix(body, "*") {
				count++
			}
		}
	}
	return count
}

// ResolvedModel is the outcome of resolving which model a session launches with.
type ResolvedModel struct {
	Model       string // the value passed to --model (never empty after resolution)
	Explicit    bool   // the task carried an explicit model choice
	Upshifted   bool   // priority upshift fired (only possible when !Explicit)
	Downshifted bool   // auto-downshift fired (only possible when !Explicit)
	Tier        string // the tier chosen when !Explicit (empty when Explicit)
}

// ResolveSessionModel decides the model a session should launch with.
//
//   - An explicit per-task model always wins and is never adjusted. This is the
//     real "appropriate model for the work" lever: whoever creates the task
//     (a person, or an autonomous agent reading the work) passes `--model` and
//     it's honored verbatim.
//   - Otherwise the baseline tier (FLOW_MODEL_TIER, default medium) is the
//     starting point, then nudged by the task's nature:
//       - high priority upshifts one rung — important autonomous work gets the
//         stronger model even when the creator didn't pin one — and is never
//         downshifted;
//       - for non-high priority, a descriptive brief downshifts one rung when
//         auto-downshift is enabled (a well-specified routine task runs cheaply).
//     The resulting tier is mapped to a provider model (Claude or Codex).
func ResolveSessionModel(provider, explicitModel, briefText, priority string) ResolvedModel {
	if m := NormalizeModel(explicitModel); m != "" {
		return ResolvedModel{Model: m, Explicit: true}
	}
	tier := ModelTierFromEnv()
	upshifted := false
	downshifted := false
	if strings.EqualFold(strings.TrimSpace(priority), "high") {
		if next := UpshiftTier(tier); next != tier {
			tier = next
			upshifted = true
		}
	} else if AutoDownshiftEnabled() && BriefIsDescriptive(briefText) {
		if next := DownshiftTier(tier); next != tier {
			tier = next
			downshifted = true
		}
	}
	return ResolvedModel{Model: ModelForTier(provider, tier), Upshifted: upshifted, Downshifted: downshifted, Tier: tier}
}
