package monitor

import "os/exec"

// lookPath is a package var so tests can stub which agent binaries "exist".
var lookPath = exec.LookPath

func providerBinary(provider string) string {
	if provider == "codex" {
		return "codex"
	}
	return "claude"
}

func providerInstalled(provider string) bool {
	_, err := lookPath(providerBinary(provider))
	return err == nil
}

// ResolveProvider decides which agent runtime to actually launch for an
// emoji/label trigger. If the requested provider is installed it's used as-is.
// If not, it falls back to the other installed provider (fellBack=true) so a
// :codex: trigger on a machine without Codex still runs as Claude rather than
// failing silently. ok=false means neither runtime is installed.
func ResolveProvider(requested string) (chosen string, fellBack bool, ok bool) {
	if requested == "" {
		requested = "claude"
	}
	if providerInstalled(requested) {
		return requested, false, true
	}
	other := "codex"
	if requested == "codex" {
		other = "claude"
	}
	if providerInstalled(other) {
		return other, true, true
	}
	return "", false, false
}

// ProviderDisplayName is the human label used in operator-facing notices.
func ProviderDisplayName(provider string) string {
	if provider == "codex" {
		return "Codex"
	}
	return "Claude Code"
}

// providerNotice appends an operator-facing note to a task's inbox (which the
// UI surfaces in the inbox feed and as a toast). Best-effort: errors are
// swallowed since the session has already been launched.
func providerNotice(slug, text string) {
	_ = AppendInboxEvent(slug, InboundEvent{Kind: "flow_notice", ChannelType: "flow", Text: text})
}
