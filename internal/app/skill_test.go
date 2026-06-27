package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse settings: %v\nraw: %s", err, raw)
	}
	return m
}

func hookEventReferencesCommand(hooks map[string]any, event, command string) bool {
	entries, _ := hooks[event].([]any)
	return countMatchingHookEntries(entries, command) >= 1
}

func countMatchingHookEntries(entries []any, command string) int {
	n := 0
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := m["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); cmd == command {
				n++
				break
			}
		}
	}
	return n
}

func expectedCommand(event string) string {
	switch event {
	case "SessionStart":
		return "flow hook session-start"
	case "UserPromptSubmit":
		return "flow hook user-prompt-submit"
	}
	return ""
}

// withTempHome redirects $HOME to a tempdir for the duration of the test.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldHome := os.Getenv("HOME")
	oldCodexHome, hadCodexHome := os.LookupEnv("CODEX_HOME")
	os.Setenv("HOME", dir)
	os.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
	t.Cleanup(func() {
		os.Setenv("HOME", oldHome)
		if hadCodexHome {
			os.Setenv("CODEX_HOME", oldCodexHome)
		} else {
			os.Unsetenv("CODEX_HOME")
		}
	})
	return dir
}

func embeddedSkillCorpus(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.Write(embeddedSkill)

	refsDir := "skill/references"
	if _, err := fs.Stat(embeddedSkillFS, refsDir); errors.Is(err, fs.ErrNotExist) {
		return b.String()
	} else if err != nil {
		t.Fatalf("stat skill references: %v", err)
	}
	if err := fs.WalkDir(embeddedSkillFS, refsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := embeddedSkillFS.ReadFile(path)
		if err != nil {
			return err
		}
		b.WriteString("\n\n")
		b.Write(data)
		return nil
	}); err != nil {
		t.Fatalf("read skill references: %v", err)
	}
	return b.String()
}

func TestEmbeddedSkillFitsProgressiveDisclosureBudget(t *testing.T) {
	const maxSkillLines = 350
	if lines := bytes.Count(embeddedSkill, []byte("\n")); lines > maxSkillLines {
		t.Fatalf("SKILL.md has %d lines, want <= %d", lines, maxSkillLines)
	}
}

func TestSkillInstallWritesFile(t *testing.T) {
	home := withTempHome(t)

	rc := cmdSkill([]string{"install"})
	if rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	path := filepath.Join(home, ".claude", "skills", "flow", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "name: flow") {
		t.Errorf("installed skill missing frontmatter 'name: flow'")
	}
	if !strings.Contains(string(data), "---") {
		t.Errorf("installed skill missing YAML frontmatter delimiters")
	}
	codexPath := filepath.Join(home, ".codex", "skills", "flow", "SKILL.md")
	codexData, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex skill: %v", err)
	}
	if !strings.Contains(string(codexData), "name: flow") {
		t.Errorf("installed codex skill missing frontmatter 'name: flow'")
	}
}

func TestSkillInstallWritesReferenceFiles(t *testing.T) {
	home := withTempHome(t)

	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	for _, base := range []string{
		filepath.Join(home, ".claude", "skills", "flow"),
		filepath.Join(home, ".codex", "skills", "flow"),
	} {
		for _, rel := range []string{
			"references/bootstrap.md",
			"references/task-intake.md",
			"references/command-reference.md",
			"references/connectors.md",
			"references/playbooks.md",
			"references/kb-closeout.md",
			"references/orchestration.md",
		} {
			path := filepath.Join(base, rel)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("installed reference missing %s: %v", path, err)
				continue
			}
			if len(bytes.TrimSpace(data)) == 0 {
				t.Errorf("installed reference %s is empty", path)
			}
		}
	}
}

func TestSkillInstallErrorsOnExisting(t *testing.T) {
	withTempHome(t)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("first install rc=%d", rc)
	}
	if rc := cmdSkill([]string{"install"}); rc == 0 {
		t.Errorf("second install without --force should fail, got rc=%d", rc)
	}
}

func TestSkillInstallForceOverwrites(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".claude", "skills", "flow", "SKILL.md")

	// Pre-create something different.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("something else"), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdSkill([]string{"install", "--force"}); rc != 0 {
		t.Fatalf("install --force rc=%d", rc)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "something else" {
		t.Error("install --force did not overwrite existing file")
	}
}

func TestSkillUpdateIsForceInstall(t *testing.T) {
	withTempHome(t)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("first install rc=%d", rc)
	}
	// `update` should succeed even though file exists.
	if rc := cmdSkill([]string{"update"}); rc != 0 {
		t.Errorf("update rc=%d, want 0", rc)
	}
}

func TestSkillUninstallRemovesDir(t *testing.T) {
	home := withTempHome(t)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	dir := filepath.Join(home, ".claude", "skills", "flow")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("skill dir missing after install: %v", err)
	}
	if rc := cmdSkill([]string{"uninstall"}); rc != 0 {
		t.Fatalf("uninstall rc=%d", rc)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("skill dir still present after uninstall: %v", err)
	}
	codexDir := filepath.Join(home, ".codex", "skills", "flow")
	if _, err := os.Stat(codexDir); !os.IsNotExist(err) {
		t.Errorf("codex skill dir still present after uninstall: %v", err)
	}
}

func TestSkillUninstallIdempotent(t *testing.T) {
	withTempHome(t)
	// Nothing installed — uninstall should still succeed.
	if rc := cmdSkill([]string{"uninstall"}); rc != 0 {
		t.Errorf("uninstall on empty home rc=%d", rc)
	}
}

// TestSkillInstallWritesSessionStartHook verifies install wires up the
// SessionStart hook into ~/.claude/settings.json. The UserPromptSubmit
// hook was retired in v0.1.0-alpha.7 — install MUST NOT add it.
func TestSkillInstallWritesSessionStartHook(t *testing.T) {
	home := withTempHome(t)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	settings := readSettings(t, filepath.Join(home, ".claude", "settings.json"))
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatal("settings.json has no hooks key")
	}
	if !hookEventReferencesCommand(hooks, "SessionStart", "flow hook session-start") {
		t.Errorf("SessionStart hook missing or wrong command: %#v", hooks["SessionStart"])
	}
	statusLine, _ := settings["statusLine"].(map[string]any)
	wantStatusLine, err := claudeStatusLineInstallCommand()
	if err != nil {
		t.Fatal(err)
	}
	if cmd, _ := statusLine["command"].(string); cmd != wantStatusLine {
		t.Errorf("statusLine command = %q, want %q", cmd, wantStatusLine)
	}
	// UserPromptSubmit must not be installed by fresh install.
	if hookEventReferencesCommand(hooks, "UserPromptSubmit", "flow hook user-prompt-submit") {
		t.Errorf("install should NOT add UserPromptSubmit hook; got %#v", hooks["UserPromptSubmit"])
	}
}

// TestSkillInstallIsIdempotent verifies a second install --force does
// not duplicate the SessionStart entry. Past regressions append
// duplicates silently; pin against that.
func TestSkillInstallIsIdempotent(t *testing.T) {
	home := withTempHome(t)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("first install rc=%d", rc)
	}
	if rc := cmdSkill([]string{"install", "--force"}); rc != 0 {
		t.Fatalf("second install --force rc=%d", rc)
	}
	settings := readSettings(t, filepath.Join(home, ".claude", "settings.json"))
	hooks, _ := settings["hooks"].(map[string]any)
	entries, _ := hooks["SessionStart"].([]any)
	if got := countMatchingHookEntries(entries, expectedCommand("SessionStart")); got != 1 {
		t.Errorf("SessionStart: got %d matching entries, want 1", got)
	}
	statusLine, _ := settings["statusLine"].(map[string]any)
	wantStatusLine, err := claudeStatusLineInstallCommand()
	if err != nil {
		t.Fatal(err)
	}
	if cmd, _ := statusLine["command"].(string); cmd != wantStatusLine {
		t.Errorf("statusLine command = %q, want %q", cmd, wantStatusLine)
	}
	if _, ok := settings[claudeStatusLinePreviousKey]; ok {
		t.Errorf("second install should not store Flow's own statusLine as previous: %#v", settings[claudeStatusLinePreviousKey])
	}
}

func TestSkillInstallPreservesExistingStatusLine(t *testing.T) {
	home := withTempHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "statusLine": {"type": "command", "command": "custom-status --fast", "padding": 2},
  "someUnrelatedKey": "preserve-me"
}`
	if err := os.WriteFile(settingsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	settings := readSettings(t, settingsPath)
	statusLine, _ := settings["statusLine"].(map[string]any)
	wantStatusLine, err := claudeStatusLineInstallCommand()
	if err != nil {
		t.Fatal(err)
	}
	if cmd, _ := statusLine["command"].(string); cmd != wantStatusLine {
		t.Fatalf("statusLine command = %q, want %q", cmd, wantStatusLine)
	}
	if padding, _ := statusLine["padding"].(float64); padding != 2 {
		t.Fatalf("statusLine padding = %v, want preserved 2", statusLine["padding"])
	}
	prev, _ := settings[claudeStatusLinePreviousKey].(map[string]any)
	if cmd, _ := prev["command"].(string); cmd != "custom-status --fast" {
		t.Fatalf("previous statusLine command = %q, want preserved custom command", cmd)
	}
	if v, _ := settings["someUnrelatedKey"].(string); v != "preserve-me" {
		t.Fatalf("unrelated key = %q, want preserved", v)
	}

	if rc := cmdSkill([]string{"uninstall"}); rc != 0 {
		t.Fatalf("uninstall rc=%d", rc)
	}
	settings = readSettings(t, settingsPath)
	restored, _ := settings["statusLine"].(map[string]any)
	if cmd, _ := restored["command"].(string); cmd != "custom-status --fast" {
		t.Fatalf("restored statusLine command = %q, want custom-status --fast", cmd)
	}
	if _, ok := settings[claudeStatusLinePreviousKey]; ok {
		t.Fatalf("previous statusLine key should be removed after restore")
	}
}

func TestSkillInstallStatusLineEmbedsFlowRoot(t *testing.T) {
	home := withTempHome(t)
	root := filepath.Join(home, "custom-flow")
	t.Setenv("FLOW_ROOT", root)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	settings := readSettings(t, filepath.Join(home, ".claude", "settings.json"))
	statusLine, _ := settings["statusLine"].(map[string]any)
	want := "FLOW_ROOT='" + root + "' flow hook claude-statusline"
	if cmd, _ := statusLine["command"].(string); cmd != want {
		t.Fatalf("statusLine command = %q, want %q", cmd, want)
	}
}

// TestSkillInstallRemovesStaleUserPromptSubmit verifies the upgrade
// path: an existing settings.json with a UserPromptSubmit hook entry
// (legacy install from <= v0.1.0-alpha.6) gets that entry removed by
// `flow skill install --force`, even on a fresh skill install.
func TestSkillInstallRemovesStaleUserPromptSubmit(t *testing.T) {
	home := withTempHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `{
  "hooks": {
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "flow hook user-prompt-submit"}]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	settings := readSettings(t, settingsPath)
	hooks, _ := settings["hooks"].(map[string]any)
	if hookEventReferencesCommand(hooks, "UserPromptSubmit", "flow hook user-prompt-submit") {
		t.Errorf("install should remove stale UserPromptSubmit hook; got %#v", hooks["UserPromptSubmit"])
	}
	// SessionStart should still get installed normally.
	if !hookEventReferencesCommand(hooks, "SessionStart", "flow hook session-start") {
		t.Errorf("SessionStart hook missing after install: %#v", hooks["SessionStart"])
	}
}

// TestSkillInstallPreservesUnrelatedHooks pins the safety property:
// removing flow's UserPromptSubmit hook MUST NOT touch other commands
// the user has wired under UserPromptSubmit (e.g. their own scripts),
// and MUST NOT touch other event keys (SessionEnd, PreToolUse, etc.).
func TestSkillInstallPreservesUnrelatedHooks(t *testing.T) {
	home := withTempHome(t)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	mixed := `{
  "hooks": {
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "flow hook user-prompt-submit"}]},
      {"hooks": [{"type": "command", "command": "my-custom-tool --watch"}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "user-pretool-script"}]}
    ]
  },
  "someUnrelatedKey": "preserve-me"
}`
	if err := os.WriteFile(settingsPath, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}

	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	settings := readSettings(t, settingsPath)

	// flow's UPS entry gone; user's UPS entry preserved.
	hooks, _ := settings["hooks"].(map[string]any)
	if hookEventReferencesCommand(hooks, "UserPromptSubmit", "flow hook user-prompt-submit") {
		t.Errorf("flow UPS hook should be removed; got %#v", hooks["UserPromptSubmit"])
	}
	if !hookEventReferencesCommand(hooks, "UserPromptSubmit", "my-custom-tool --watch") {
		t.Errorf("user's UPS hook MUST be preserved; got %#v", hooks["UserPromptSubmit"])
	}

	// PreToolUse untouched.
	if !hookEventReferencesCommand(hooks, "PreToolUse", "user-pretool-script") {
		t.Errorf("PreToolUse hook MUST be preserved; got %#v", hooks["PreToolUse"])
	}

	// Top-level non-hook key untouched.
	if v, _ := settings["someUnrelatedKey"].(string); v != "preserve-me" {
		t.Errorf("unrelated top-level key should be preserved, got %v", settings["someUnrelatedKey"])
	}
}

// TestSkillUninstallRemovesSessionStartHook verifies uninstall strips
// the SessionStart entry. (The UserPromptSubmit hook is no longer
// installed, so its absence after uninstall is verified by the
// install-side tests.)
func TestSkillUninstallRemovesSessionStartHook(t *testing.T) {
	home := withTempHome(t)
	if rc := cmdSkill([]string{"install"}); rc != 0 {
		t.Fatalf("install rc=%d", rc)
	}
	if rc := cmdSkill([]string{"uninstall"}); rc != 0 {
		t.Fatalf("uninstall rc=%d", rc)
	}
	settings := readSettings(t, filepath.Join(home, ".claude", "settings.json"))
	hooks, _ := settings["hooks"].(map[string]any)
	if len(hooks) != 0 {
		t.Errorf("expected hooks map empty or absent after uninstall, got %#v", hooks)
	}
}

// TestSkillInstallSkipHook leaves settings.json untouched when --skip-hook.
func TestSkillInstallSkipHook(t *testing.T) {
	home := withTempHome(t)
	if rc := cmdSkill([]string{"install", "--skip-hook"}); rc != 0 {
		t.Fatalf("install --skip-hook rc=%d", rc)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("--skip-hook should not create settings.json; stat err=%v", err)
	}
}

func TestSkillUnknownSubcommand(t *testing.T) {
	if rc := cmdSkill([]string{"wat"}); rc != 2 {
		t.Errorf("unknown subcommand rc=%d, want 2", rc)
	}
	if rc := cmdSkill(nil); rc != 2 {
		t.Errorf("missing subcommand rc=%d, want 2", rc)
	}
}

func TestSkillMentionsPlaybooks(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"## 2. The model",
		"**Playbooks**",
		"flow add playbook",
		"flow run playbook",
		"flow list playbooks",
		"flow show playbook",
		"flow list runs",
		"Active playbooks",
		"playbooks/<slug>/updates/",
		"playbook definitions are never \"done\" — they're archived",
		"flow archive <playbook-slug>",
		"## Playbook activity",
		"Each run does",
		"Signals to watch for",
		"Do not *ad-hoc* auto-fire `flow run playbook`",
		"snapshot",
		"Schedule a playbook (recurring runs)",
		"autonomously in `--auto` mode",
		"the bootstrapped task\" includes playbook-run tasks",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillDocumentsCodexAutoRuns(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Headless background run",
		"Claude or Codex",
		"codex exec",
		"workspace-write sandbox",
		"task's provider and resolved model",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Codex auto-run guidance %q", want)
		}
	}
	for _, stale := range []string{
		"--auto is claude-only",
		"claude-only (D1)",
		"offer Regular or Skip instead",
	} {
		if strings.Contains(got, stale) {
			t.Errorf("skill still contains stale Claude-only autonomous guidance %q", stale)
		}
	}
}

func TestSkillMentionsDMMonitoring(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Monitoring a DM reply (automatic)",
		"PostToolUse",
		"DM thread you started",
		"events on behalf of users",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing DM-monitoring guidance %q", want)
		}
	}
	// The removed manual-tag instruction must not linger.
	if strings.Contains(got, "--tag slack-dm:") {
		t.Errorf("skill still instructs the removed slack-dm manual tag")
	}
}

func TestSkillDocumentsSlackCommandEyesAck(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Slack app command DM",
		"`:eyes:` reaction",
		"operator's own DM to the Flow Slack app",
		"left in place",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Slack command eyes ack guidance %q", want)
		}
	}
}

func TestSkillDocumentsSlackConnectSafeReplyPath(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Slack Connect",
		"flow slack send",
		"--as user",
		"--thread-ts",
		"--text-file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Slack Connect-safe reply guidance %q", want)
		}
	}
}

func TestSkillDocumentsSlackReadFallback(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"standalone read fallback",
		"flow slack history",
		"flow slack thread",
		"flow slack search-users",
		"flow slack search <query>",
		"search:read",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Slack read fallback guidance %q", want)
		}
	}
}

func TestSkillDoesNotInstructCodexSlackFooter(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, bad := range []string{
		"Sent using @codex",
		"Always append this footer to every Codex-sent Slack reply",
		"you MUST append your own footer",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("skill still instructs manual Codex Slack attribution footer: %q", bad)
		}
	}
	for _, want := range []string{
		"Do not add a manual `Sent using ...` footer",
		"Slack/ChatGPT app attribution",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing no-manual-footer guidance %q", want)
		}
	}
}

func TestSkillMentionsSoftDelete(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"delete/remove/trash",
		"flow delete <ref>",
		"flow restore <ref>",
		"--include-deleted",
		"--deleted",
		"Soft-delete",
		"Archive vs delete",
		"wrong thing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillDocumentsMemorySearch(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"### 4.9a Search flow memory",
		"searches briefs, updates, and memories by",
		"flow KB files under `~/.flow/kb/`, Codex",
		"Claude auto-memory markdown",
		"use `--in transcripts` for transcript-only search or `--in all`",
		"Search is a locator, not an authority",
		"`flow search \"<terms>\" --in memories`",
		"Search is compatible with lazy loading",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing memory-search guidance %q", want)
		}
	}
}

func TestSkillHasPlaybookSections(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"### 4.12 Add a playbook",
		"### 4.13 Run a playbook",
		"fire the X agent",
		"kind: playbook_run",
		"snapshot taken when this run started",
		"Files listed under `other:`",
		"load on demand",
		"Auxiliary files in entity directories",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillDocumentsTaskArtifactsDirectory(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Task artifacts directory",
		"$FLOW_ROOT/tasks/<task-slug>/artifacts/",
		"Mission Control's task **Artifacts** tab is driven by",
		"Do not write new deliverables as top-level files next",
		"top-level `.md` files are `other:` context, not UI artifacts",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing task artifacts guidance %q", want)
		}
	}
}

func TestSkillSection414(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"### 4.14 Substantive-unrelated-work check",
		"ongoing check, not one-shot",
		"superpowers:brainstorming",
		"Re-evaluate on every turn",
		"Process-skill ordering",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillIntakeMinimal(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Required sections (always asked, in this order)",
		"Optional sections (offered, can be deferred)",
		"Detail now",
		"Defer until you start the task",
		"Thin task brief (intake-minimal)",
		"*Deferred — fill in at task start.*",
		"Deferred-section prompt",
		"Fill in now",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillIntakeSurfacesDueDateAndAssignee(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"**Assignee**",
		"Me (self)",
		"pass `--assignee <name>` only when",
		"**Due date**",
		"No due date",
		"pass `--due <date>` only when",
		"always ask, easy to skip",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing task-intake due/assignee guidance %q", want)
		}
	}
}

func TestSkillUsesAskUserQuestionConsistently(t *testing.T) {
	got := embeddedSkillCorpus(t)
	// The skill should have many AskUserQuestion references — at least one
	// per major workflow that involves user choice.
	count := strings.Count(got, "AskUserQuestion")
	if count < 40 {
		t.Errorf("expected at least 40 AskUserQuestion references in skill, got %d", count)
	}
	// §4a should set the policy explicitly.
	if !strings.Contains(got, "always AskUserQuestion") {
		t.Errorf("skill §4a should establish 'always AskUserQuestion' as the rule")
	}
}

func TestSkillHasPlaybookPersistAdjustmentsPattern(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Persisting in-run adjustments back to the playbook",
		"frozen snapshot",
		"playbooks/<slug>/brief.md",
		"Persist to playbook",
		"Just this run",
		"Never edit the run-task's own `brief.md`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing %q", want)
		}
	}
}

func TestSkillHasMidInterviewDriftRule(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Mid-interview drift",
		"sub-question has 2–4 discrete options",
		"Don't keep typing prose just because you started",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing mid-interview-drift content %q", want)
		}
	}
}

// TestSkillHasUpgradeWorkflow pins the §4.15 upgrade procedure: the
// skill must know how to walk the user through replacing the binary
// per the README at https://github.com/Facets-cloud/flow and then
// running `flow skill update`. It must also recognize the
// `flow-version-stale:` signal the SessionStart hook emits when the
// local binary lags the latest GitHub release.
func TestSkillHasUpgradeWorkflow(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"### 4.15 Upgrade flow itself",
		"https://github.com/Facets-cloud/flow",
		"flow --version",
		"flow skill update",
		"flow-version-stale:",
		"xattr -d com.apple.quarantine",
		"Do not invent download URLs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing upgrade workflow content: %q", want)
		}
	}
}

// TestSkillEmphasizesCloseOutValue pins §4.7's framing of `flow done`
// as the load-bearing moment that persists the session's learnings —
// it triggers the close-out sweep that writes KB + project update.
// Without this content the skill treats closure as bookkeeping and
// Claude never proactively offers to close, which means the user's
// learnings stay locked in the transcript.
func TestSkillEmphasizesCloseOutValue(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Why closing matters",
		"close-out sweep",
		"that distillation never happens",
		"flow-<slug>",
		"schedules a delayed tmux close",
		"silent loss of durable knowledge",
		"Recognizing natural close-out moments",
		// Expanded trigger list must include real-world wrap-up phrasing,
		// not just the literal verbs the old skill listed.
		"shipped",
		"PR merged",
		"deployed",
		"that's working",
		// Matching §8 anti-pattern reinforces the rule.
		"Do not let work wrap up without prompting closure",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing close-out emphasis: %q", want)
		}
	}
}

// TestSkillDocumentsCloseoutKBUpgrade pins §4.10's scoped exception to append-only:
// at close-out, completed work may supersede a provisional KB entry (a plan now
// executed) in place, so the always-loaded KB doesn't carry stale plans forever.
func TestSkillDocumentsCloseoutKBUpgrade(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Real-time scoop is append-only",
		"Exception — close-out upgrade",
		"supersede that entry in place",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing close-out KB-upgrade rule: %q", want)
		}
	}
}

// TestSkillHasAccessibilityErrorRecipe pins the §4.4 recipe for
// handling the macOS Accessibility error from the Terminal.app
// backend: name Terminal definitively (not Claude/flow), open the
// right Settings pane via the deep-link URL, and retry only after
// explicit user confirmation.
func TestSkillHasAccessibilityErrorRecipe(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"macOS Accessibility error from the Terminal.app backend",
		"Trust the error verbatim",
		"NOT Claude Code",
		"NOT the flow binary",
		"x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility",
		"there is no CLI to",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Accessibility recipe: %q", want)
		}
	}
}

// TestSkillHasExplicitInvocationSection pins the §1a behavior: when
// the skill is invoked without a trigger phrase, it should describe
// its capabilities and AskUserQuestion for the user's intent — NOT
// auto-run §4.1, auto-list tasks, or auto-propose opening a task.
func TestSkillHasExplicitInvocationSection(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"## 1a. When invoked explicitly with no intent",
		"DO NOT auto-run any workflow",
		"do not enter §4.1",
		`What now?`,
		"Just exploring",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing §1a content: %q", want)
		}
	}
}

// TestSkillNoCliCoachingInUserFacingLabels pins the rule that the
// skill must not put literal `flow ...` invocations inside
// AskUserQuestion option labels or chat replies. Users should never
// see CLI commands; Claude uses flow under the hood.
//
// We pin two specific past offenders that motivated the sweep — the
// "Run init?" prompt that read "Yes, run flow init", and the
// "Mark done?" prompt that read "Yes, `flow done <slug>`". If either
// regresses, a future sweep loses ground silently.
func TestSkillNoCliCoachingInUserFacingLabels(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, banned := range []string{
		`"Yes, run flow init"`,
		"\"Yes, `flow done <slug>`\"",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("user-facing label still exposes CLI: %s", banned)
		}
	}
	// And the rule itself must be present in §8.
	for _, want := range []string{
		"Do not surface flow commands to the user",
		"users never need to learn the CLI",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing CLI-coaching anti-pattern: %q", want)
		}
	}
}

func TestSkillHasFirstRunCapturePattern(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"First-run capture",
		"FIRST RUN OF THIS PLAYBOOK",
		"crystallizes",
		"Save as playbook reference",
		"Capture anything from this run back to the playbook",
		"Capture-back is a primary deliverable of the first run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing first-run capture content %q", want)
		}
	}
}

func TestSkillDocumentsGitHubMonitorBootstrap(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"GitHub PR and issue tasks",
		// App-based webhook ingress (the legacy gh search-poller was removed).
		"Connect GitHub",
		"App-manifest flow",
		"OS keyring",
		"/app/hook/deliveries",
		"gh-pr:<owner>/<repo>#<number>",
		"inbox.jsonl",
		"tail -F ~/.flow/tasks/<your-slug>/inbox.jsonl",
		"pr_head_updated",
		"pr_merged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing GitHub monitor bootstrap content %q", want)
		}
	}
}

func TestSkillDocumentsSameSessionInboxMonitor(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"inbox.jsonl",
		"same Flow-owned terminal session",
		"Slack, GitHub, or future source",
		"Codex",
		"gh-pr:",
		"Claude Code may offer native background-session commands",
		"Codex tasks use that same terminal wake path",
		"do not assume a Codex-native `/bg`, scheduler, or app-server/remote-control integration",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing same-session inbox monitor content %q", want)
		}
	}
}

func TestSkillDocumentsAttentionConfirmedHandoff(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"Respond to Attention confirmed handoffs",
		"Confirmed handoff request from the attention router",
		"Correlation ID",
		"flow attention handoff accept <correlation-id> --reason",
		"flow attention handoff decline <correlation-id> --reason",
		"Pending handoffs time out explicitly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Attention confirmed handoff content %q", want)
		}
	}
}

func TestSkillDocumentsAttentionWorkflow(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"flow attention list [--status new|acted|dismissed|snoozed|all]",
		"flow attention act <id> <make-task|forward|confirm-handoff|dismiss>",
		"flow attention sent <id> [--close-floating <floating-id>]",
		"flow attention trace [--since 24h] [--disposition dropped|surfaced|error|all] [--limit 50]",
		"## 10d. Attention Router feed",
		"Before asking \"what should I work on\"",
		"When an inbox/monitor event wakes you",
		"`flow attention trace` is the audit trail",
		"Do not post a send-reply yourself",
		"operator approved the reply",
		"mark the card sent only after the post is confirmed",
		"`flow attention sent <id> --close-floating <floating-id>`",
		"Default autonomy is surface-only",
		"The autonomy gate evaluates the **calibrated** confidence",
		"The autonomy trust ladder is",
		"Four **safe** actions are auto-actable in settings today",
		"Reply/AFK (outward sends) stay manual-only",
		"learned feedback never enables an action",
		"flow attention feedback --group",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing Attention workflow content %q", want)
		}
	}
}

func TestSkillDocumentsStandupBriefing(t *testing.T) {
	got := embeddedSkillCorpus(t)
	for _, want := range []string{
		"flow standup [--for today|monday|24h] [--clipboard]",
		"generate a copyable briefing from Attention, waiting, stale, ready, and recent activity",
		"### 4.1a Copyable standup / briefing",
		"daily digest",
		"separates **needs action** from **FYI**",
		"This does not replace the interactive start-the-day picker",
		"AskUserQuestion instead of only dumping a briefing",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("skill missing standup briefing content %q", want)
		}
	}
}

func TestReadmeDocumentsSameSessionProviderCapability(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"Claude Code's native background sessions are separate from Flow's monitor",
		"Codex currently exposes experimental app-server/remote-control building blocks",
		"task-local inbox monitor + Flow-owned terminal wake",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("README missing same-session provider capability content %q", want)
		}
	}
}

func TestPlaybookRunBootstrapMentionsPersistAdjustments(t *testing.T) {
	prompt := buildPlaybookRunBootstrapPrompt("p--2026-04-30-10-30", "p", false)
	for _, want := range []string{
		"adjusts the playbook",
		"AskUserQuestion",
		"Persist to playbook",
		"playbooks/p/brief.md",
		"frozen snapshot",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("playbook-run bootstrap prompt missing %q; got:\n%s", want, prompt)
		}
	}
}
