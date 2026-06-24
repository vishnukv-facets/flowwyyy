package product

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeSkillIncludesCoreAndProduct(t *testing.T) {
	got := ComposeSkill([]byte("## 1. What flow is\ncore\n"))
	text := string(got)
	for _, want := range []string{
		"## 1. What flow is",
		"## Product extensions (flowwyyy)",
		"## 10d. Attention Router feed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("composed skill missing %q", want)
		}
	}
}

// TestSkillInstallWritesComposedSkill is the binary-level regression guard for
// the T10↔T11 wiring: `flowwyyy skill install` MUST write the full composed
// (core + product) skill, not the core-only fragment a passthrough to the core
// binary would install. It exercises the registered product skill handler and
// reads back the installed file.
func TestSkillInstallWritesComposedSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FLOW_ROOT", t.TempDir())
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	if rc := cmdSkill([]string{"install", "--skip-hook"}); rc != 0 {
		t.Fatalf("skill install rc=%d (want 0)", rc)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "flow", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"## 1. What flow is",          // core fragment
		"## 10d. Attention Router feed", // product fragment
		"## 10e. Owners",                // product fragment
		"## Product extensions (flowwyyy)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("installed skill missing %q — flowwyyy must install the COMPOSED skill, not core-only", want)
		}
	}
}
