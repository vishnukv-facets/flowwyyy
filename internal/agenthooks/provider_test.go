package agenthooks

import (
	"strings"
	"testing"
)

// TestProviderRegistryDefaults pins the built-in providers so a refactor
// can't accidentally drop one. Claude and Codex must always be in the
// default registry, in deterministic order.
func TestProviderRegistryDefaults(t *testing.T) {
	got := Providers()
	if len(got) < 2 {
		t.Fatalf("Providers() returned %d, want at least 2", len(got))
	}
	if got[0].Name() != "claude" {
		t.Fatalf("first provider = %s, want claude", got[0].Name())
	}
	if got[1].Name() != "codex" {
		t.Fatalf("second provider = %s, want codex", got[1].Name())
	}
}

// TestProviderHookCommandStampsVersion confirms every Provider emits a
// command that carries --hook-version CurrentHookVersion. Without this,
// future providers might forget to opt in.
func TestProviderHookCommandStampsVersion(t *testing.T) {
	for _, p := range Providers() {
		cmd := p.HookCommand(InstallOptions{HookURL: "http://127.0.0.1:8787/api/hooks/agent"})
		if v := HookVersionFromCommand(cmd); v != CurrentHookVersion {
			t.Errorf("%s command --hook-version = %d, want %d in: %q",
				p.Name(), v, CurrentHookVersion, cmd)
		}
		if !strings.Contains(cmd, "--provider "+p.Name()) {
			t.Errorf("%s command missing --provider %s: %q", p.Name(), p.Name(), cmd)
		}
		if !strings.Contains(cmd, "http://127.0.0.1:8787/api/hooks/agent") {
			t.Errorf("%s command missing --url stamping: %q", p.Name(), cmd)
		}
	}
}

func TestCodexHookCommandRequiresFlowOwnedSession(t *testing.T) {
	cmd := codexProvider{}.HookCommand(InstallOptions{HookURL: "http://127.0.0.1:8787/api/hooks/agent"})
	for _, want := range []string{"FLOW_HOOK_OWNED", "exec flow hook agent-event", "--provider codex"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("codex hook command missing %q: %q", want, cmd)
		}
	}
}

// Extension via a dynamic RegisterProvider was removed as YAGNI — the two
// in-tree providers (claude, codex) are wired statically in provider.go.
