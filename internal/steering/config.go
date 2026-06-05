package steering

import (
	"database/sql"
	"os"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// WatchConfigFromEnv builds a WatchConfig from environment configuration. The
// operator's identity and mention IDs come from the same FLOW_SLACK_SELF_*
// vars the reaction pipeline uses (monitor.SelfUserIDs). Watched/muted
// channels and muted keywords come from FLOW_STEERING_* vars. Channel
// selection via the Mission Control settings UI is P1.4; these env vars
// bridge until then.
func WatchConfigFromEnv() WatchConfig {
	self := monitor.SelfUserIDs()
	return WatchConfig{
		WatchedChannels: toSet(splitList(os.Getenv("FLOW_STEERING_WATCH_CHANNELS"))),
		MutedChannels:   toSet(splitList(os.Getenv("FLOW_STEERING_MUTED_CHANNELS"))),
		MutedKeywords:   splitList(os.Getenv("FLOW_STEERING_MUTED_KEYWORDS")),
		Identity:        OperatorIdentity{UserIDs: self},
		MentionUserIDs:  self,
		// Reuse the operator's GitHub login(s) from the existing self-echo
		// standdown source (FLOW_GH_SELF_LOGINS) so the GitHub connector drops
		// the operator's own events. Empty is fine (→ no self-drop).
		GitHubIdentity: monitor.GitHubSelfLogins(),
	}
}

// WatchConfigFnWithMutes returns a ConfigFn that overlays the operator's durable
// "perma drop" suppressions (steering_mutes) on top of the env-derived config,
// re-read on every event so a mute set from a feed card takes effect on the next
// message with no restart. DB-sourced muted channels merge with the env ones;
// muted authors/threads come only from the table. A nil db falls back to env.
func WatchConfigFnWithMutes(db *sql.DB) func() WatchConfig {
	return func() WatchConfig {
		cfg := WatchConfigFromEnv()
		if db == nil {
			return cfg
		}
		mutes, err := flowdb.ListSteeringMutes(db)
		if err != nil {
			return cfg // on error, fall back to env-only rather than failing the cascade
		}
		if len(mutes.Channels) > 0 {
			if cfg.MutedChannels == nil {
				cfg.MutedChannels = map[string]bool{}
			}
			for ch := range mutes.Channels {
				cfg.MutedChannels[ch] = true
			}
		}
		cfg.MutedAuthors = mutes.Authors
		cfg.MutedThreads = mutes.Threads
		return cfg
	}
}

// splitList splits a comma/space/tab/newline-separated env value into trimmed,
// non-empty tokens.
func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toSet builds a membership set, returning nil for an empty input so callers
// can range/lookup uniformly (a nil map reads as "contains nothing").
func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}
