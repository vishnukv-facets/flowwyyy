package monitor

import (
	"errors"
	"testing"
)

func withInstalled(t *testing.T, installed map[string]bool) {
	t.Helper()
	orig := lookPath
	lookPath = func(bin string) (string, error) {
		if installed[bin] {
			return "/usr/local/bin/" + bin, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = orig })
}

func TestResolveProviderUsesRequestedWhenInstalled(t *testing.T) {
	withInstalled(t, map[string]bool{"claude": true, "codex": true})
	chosen, fellBack, ok := ResolveProvider("codex")
	if !ok || fellBack || chosen != "codex" {
		t.Fatalf("got chosen=%q fellBack=%v ok=%v", chosen, fellBack, ok)
	}
}

func TestResolveProviderFallsBackWhenRequestedMissing(t *testing.T) {
	withInstalled(t, map[string]bool{"claude": true, "codex": false})
	chosen, fellBack, ok := ResolveProvider("codex")
	if !ok || !fellBack || chosen != "claude" {
		t.Fatalf("expected fallback to claude; got chosen=%q fellBack=%v ok=%v", chosen, fellBack, ok)
	}
}

func TestResolveProviderFallsBackToCodexWhenClaudeMissing(t *testing.T) {
	withInstalled(t, map[string]bool{"claude": false, "codex": true})
	chosen, fellBack, ok := ResolveProvider("claude")
	if !ok || !fellBack || chosen != "codex" {
		t.Fatalf("expected fallback to codex; got chosen=%q fellBack=%v ok=%v", chosen, fellBack, ok)
	}
}

func TestResolveProviderFailsWhenNeitherInstalled(t *testing.T) {
	withInstalled(t, map[string]bool{"claude": false, "codex": false})
	if _, _, ok := ResolveProvider("codex"); ok {
		t.Fatal("expected ok=false when neither runtime installed")
	}
}

func TestResolveProviderEmptyDefaultsToClaude(t *testing.T) {
	withInstalled(t, map[string]bool{"claude": true, "codex": true})
	chosen, fellBack, ok := ResolveProvider("")
	if !ok || fellBack || chosen != "claude" {
		t.Fatalf("got chosen=%q fellBack=%v ok=%v", chosen, fellBack, ok)
	}
}
