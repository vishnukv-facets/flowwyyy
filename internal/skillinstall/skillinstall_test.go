package skillinstall_test

// skillinstall is a port of app's skill-install machinery (parameterized by
// Config). app/skill_test.go proves the original; this proves the port behaves
// identically end-to-end: install writes the content + version sidecar + the
// SessionStart hook to both the Claude and Codex skill paths, a second install
// without --force is rejected, and uninstall removes the skill + hook.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flow/internal/skillinstall"
)

func TestRunInstallThenUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)

	content := []byte("---\nname: flow\n---\nSKILL BODY\n")
	cfg := skillinstall.Config{Content: content, Version: "v9.9.9"}

	if rc := skillinstall.Run([]string{"install"}, cfg); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}

	claudePath := filepath.Join(home, ".claude", "skills", "flow", "SKILL.md")
	if b, err := os.ReadFile(claudePath); err != nil || string(b) != string(content) {
		t.Fatalf("claude skill: err=%v content=%q want %q", err, b, content)
	}
	codexPath := filepath.Join(codexHome, "skills", "flow", "SKILL.md")
	if b, err := os.ReadFile(codexPath); err != nil || string(b) != string(content) {
		t.Fatalf("codex skill: err=%v content=%q", err, b)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "flow", "VERSION")); err != nil || strings.TrimSpace(string(b)) != "v9.9.9" {
		t.Fatalf("version sidecar: err=%v content=%q want v9.9.9", err, b)
	}

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	settings, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(settings), "flow hook session-start") || !strings.Contains(string(settings), "SessionStart") {
		t.Fatalf("SessionStart hook not wired: err=%v body=%s", err, settings)
	}

	// Second install without --force must be rejected; --force/update must not.
	if rc := skillinstall.Run([]string{"install"}, cfg); rc == 0 {
		t.Errorf("second install without --force should fail")
	}
	if rc := skillinstall.Run([]string{"update"}, cfg); rc != 0 {
		t.Errorf("update (force install) rc=%d", rc)
	}
	if rc := skillinstall.Run([]string{"print"}, cfg); rc != 0 {
		t.Errorf("print rc=%d", rc)
	}

	if rc := skillinstall.Run([]string{"uninstall"}, cfg); rc != 0 {
		t.Fatalf("uninstall rc=%d", rc)
	}
	if _, err := os.Stat(claudePath); !os.IsNotExist(err) {
		t.Errorf("claude skill still present after uninstall: %v", err)
	}
	if b, _ := os.ReadFile(settingsPath); strings.Contains(string(b), "flow hook session-start") {
		t.Errorf("SessionStart hook still present after uninstall: %s", b)
	}
}
