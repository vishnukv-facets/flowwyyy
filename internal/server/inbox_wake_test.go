package server

import (
	"testing"

	"flow/internal/monitor"
)

// untrusted is a stamped untrusted (connector) inbox entry with the given
// trusted-source + calibrated-confidence stamp.
func stampedUntrusted(trusted bool, conf float64) monitor.InboxEntry {
	return monitor.InboxEntry{
		Event: monitor.InboundEvent{Kind: "issue_comment", ChannelType: "github", Text: "hi"},
		Meta:  monitor.InboxEventMeta{Source: "github", Actionable: true, TrustedSource: trusted, CalibratedConfidence: conf},
	}
}

// trustedFlow is a flow_tell entry (operator/parent coordination) — always
// delivered, never gated by auto-permit.
func trustedFlow() monitor.InboxEntry {
	return monitor.InboxEntry{
		Event: monitor.InboundEvent{Kind: "flow_tell", ChannelType: "flow", Text: "proceed"},
		Meta:  monitor.InboxEventMeta{Source: "flow", Actionable: true},
	}
}

// legacyUntrusted is an unstamped connector row (empty meta) — the source is
// recoverable by reclassification but the stamp is not, so it must fail closed.
func legacyUntrusted() monitor.InboxEntry {
	return monitor.InboxEntry{Event: monitor.InboundEvent{Kind: "issue_comment", ChannelType: "github", Text: "hi"}}
}

func TestEntriesAutoPermitted(t *testing.T) {
	const minConf = 0.90
	cases := []struct {
		name    string
		enabled bool
		entries []monitor.InboxEntry
		want    bool
	}{
		{"disabled withholds even when trusted+high", false, []monitor.InboxEntry{stampedUntrusted(true, 0.99)}, false},
		{"enabled low-confidence withholds", true, []monitor.InboxEntry{stampedUntrusted(true, 0.80)}, false},
		{"enabled untrusted-source withholds", true, []monitor.InboxEntry{stampedUntrusted(false, 0.99)}, false},
		{"enabled trusted+high delivers", true, []monitor.InboxEntry{stampedUntrusted(true, 0.91)}, true},
		{"enabled trusted at exactly the floor delivers", true, []monitor.InboxEntry{stampedUntrusted(true, minConf)}, true},
		{"enabled legacy unstamped withholds (fail closed)", true, []monitor.InboxEntry{legacyUntrusted()}, false},
		{"enabled only trusted-flow has nothing to permit", true, []monitor.InboxEntry{trustedFlow()}, false},
		{"enabled mixed: one bad untrusted withholds whole batch", true, []monitor.InboxEntry{stampedUntrusted(true, 0.99), stampedUntrusted(true, 0.50)}, false},
		{"enabled trusted untrusted alongside flow delivers", true, []monitor.InboxEntry{stampedUntrusted(true, 0.95), trustedFlow()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entriesAutoPermitted(tc.entries, tc.enabled, minConf); got != tc.want {
				t.Fatalf("entriesAutoPermitted(enabled=%v) = %v, want %v", tc.enabled, got, tc.want)
			}
		})
	}
}

func TestAutoPermitUnattendedConfig(t *testing.T) {
	cases := []struct {
		name        string
		optIn       string
		conf        string
		wantEnabled bool
		wantConf    float64
	}{
		{"unset is disabled with default floor", "", "", false, defaultAutoPermitMinConf},
		{"true with explicit floor", "true", "0.8", true, 0.8},
		{"true with invalid floor falls back to default", "true", "nope", true, defaultAutoPermitMinConf},
		{"true with out-of-range floor falls back to default", "true", "1.5", true, defaultAutoPermitMinConf},
		{"garbage opt-in is disabled", "garbage", "0.8", false, 0.8},
		{"explicit false is disabled", "false", "", false, defaultAutoPermitMinConf},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLOW_STEERING_AUTO_PERMIT_UNATTENDED", tc.optIn)
			t.Setenv("FLOW_STEERING_AUTO_PERMIT_MIN_CONF", tc.conf)
			gotEnabled, gotConf := autoPermitUnattendedConfig()
			if gotEnabled != tc.wantEnabled || gotConf != tc.wantConf {
				t.Fatalf("autoPermitUnattendedConfig() = (%v, %.2f), want (%v, %.2f)", gotEnabled, gotConf, tc.wantEnabled, tc.wantConf)
			}
		})
	}
}
