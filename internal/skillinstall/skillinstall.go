// Package skillinstall installs/updates/uninstalls/prints the flow agent skill
// (the SKILL.md under ~/.claude/skills/flow and ~/.codex/skills/flow) and wires
// the SessionStart hook in ~/.claude/settings.json.
//
// It is flowwyyy's OWN copy of the skill-install machinery, parameterized by the
// skill CONTENT and binary VERSION (the only things that differ per binary): the
// flowwyyy product binary installs the COMPOSED core+product skill, while the
// core `flow` binary keeps its own equivalent under internal/app. In the
// two-binary world the official `flow` and `flowwyyy` never share code, so this
// duplication is by design (Phase-3 decoupling, seam §11.3.1, Tier C). The
// package is pure stdlib + internal/cli — no flowdb, no app.
package skillinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"flow/internal/cli"
)

// Config carries the per-binary inputs: the skill bytes to install/print and
// the binary version recorded in the install sidecar.
type Config struct {
	Content []byte
	Version string
}

// hookCommand is the exact string written into settings.json under
// hooks.SessionStart so install/uninstall can idempotently find it. Keep it
// stable — changing it would orphan existing installations. It is "flow ..."
// (not "flowwyyy ...") because the SessionStart hook invokes the core hook
// handler, which both binaries expose at the same verb.
const hookCommand = "flow hook session-start"

// hookMatcher is the SessionStart matcher — fires on fresh startup and resume.
const hookMatcher = "startup|resume"

// userPromptSubmitHookCommand is the (removed) UserPromptSubmit hook command;
// install/uninstall actively strip any stale entry left by older binaries.
const userPromptSubmitHookCommand = "flow hook user-prompt-submit"

// Run dispatches `skill install|update|uninstall|print` using cfg.
func Run(args []string, cfg Config) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: skill requires a subcommand (install|uninstall|update|print)")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return install(rest, cfg, false)
	case "update":
		return install(rest, cfg, true)
	case "uninstall":
		return uninstall(rest)
	case "print":
		if len(rest) != 0 {
			fmt.Fprintln(os.Stderr, "error: skill print takes no arguments")
			return 2
		}
		if _, err := os.Stdout.Write(cfg.Content); err != nil {
			fmt.Fprintf(os.Stderr, "error: write skill: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "error: unknown skill subcommand %q\n", sub)
		return 2
	}
}

func install(args []string, cfg Config, forceDefault bool) int {
	fs := cli.FlagSet("skill install")
	force := fs.Bool("force", forceDefault, "overwrite an existing installation")
	skipHook := fs.Bool("skip-hook", false, "don't auto-install the SessionStart hook in ~/.claude/settings.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dests, err := skillInstallPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	for _, dest := range dests {
		if _, err := os.Stat(dest); err == nil && !*force {
			fmt.Fprintf(os.Stderr, "error: %s already exists; use --force to overwrite\n", dest)
			return 1
		} else if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error: stat %s: %v\n", dest, err)
			return 1
		}
	}
	for _, dest := range dests {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "error: create %s: %v\n", filepath.Dir(dest), err)
			return 1
		}
		if err := os.WriteFile(dest, cfg.Content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write %s: %v\n", dest, err)
			return 1
		}
		fmt.Printf("installed flow skill to %s\n", dest)
	}
	if err := writeSkillVersion(cfg.Version); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record skill version: %v\n", err)
	}

	if *skipHook {
		fmt.Println("--skip-hook: leaving ~/.claude/settings.json alone")
		return 0
	}
	if added, err := installSessionStartHook(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install SessionStart hook: %v\n", err)
		return 0
	} else if added {
		settings, _ := userSettingsPath()
		fmt.Printf("installed SessionStart hook in %s (fires on startup + resume)\n", settings)
	} else {
		fmt.Println("SessionStart hook already installed — leaving as is")
	}
	if removed, err := uninstallUserPromptSubmitHook(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove stale UserPromptSubmit hook: %v\n", err)
		return 0
	} else if removed {
		settings, _ := userSettingsPath()
		fmt.Printf("removed stale UserPromptSubmit hook from %s (no longer used)\n", settings)
	}
	return 0
}

func uninstall(args []string) int {
	fs := cli.FlagSet("skill uninstall")
	keepHook := fs.Bool("keep-hook", false, "don't remove the SessionStart hook from ~/.claude/settings.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dests, err := skillInstallPaths()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	removedAny := false
	for _, dest := range dests {
		skillDir := filepath.Dir(dest)
		if _, err := os.Stat(skillDir); os.IsNotExist(err) {
			continue
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "error: stat %s: %v\n", skillDir, err)
			return 1
		}
		if err := os.RemoveAll(skillDir); err != nil {
			fmt.Fprintf(os.Stderr, "error: remove %s: %v\n", skillDir, err)
			return 1
		}
		fmt.Printf("uninstalled flow skill from %s\n", skillDir)
		removedAny = true
	}
	if !removedAny {
		fmt.Println("flow skill not installed — nothing to do")
	}

	if *keepHook {
		fmt.Println("--keep-hook: leaving SessionStart hook in settings.json")
		return 0
	}
	if removed, err := uninstallSessionStartHook(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove SessionStart hook: %v\n", err)
		return 0
	} else if removed {
		settings, _ := userSettingsPath()
		fmt.Printf("removed SessionStart hook from %s\n", settings)
	}
	if removed, err := uninstallUserPromptSubmitHook(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove UserPromptSubmit hook: %v\n", err)
		return 0
	} else if removed {
		settings, _ := userSettingsPath()
		fmt.Printf("removed UserPromptSubmit hook from %s\n", settings)
	}
	return 0
}

// ---------- install paths + version sidecar ----------

func skillInstallPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "skills", "flow", "SKILL.md"), nil
}

func codexSkillInstallPath() (string, error) {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "skills", "flow", "SKILL.md"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home dir: %w", err)
	}
	return filepath.Join(home, ".codex", "skills", "flow", "SKILL.md"), nil
}

func skillInstallPaths() ([]string, error) {
	claudePath, err := skillInstallPath()
	if err != nil {
		return nil, err
	}
	codexPath, err := codexSkillInstallPath()
	if err != nil {
		return nil, err
	}
	if codexPath == claudePath {
		return []string{claudePath}, nil
	}
	return []string{claudePath, codexPath}, nil
}

func skillVersionPath() (string, error) {
	skill, err := skillInstallPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(skill), "VERSION"), nil
}

func writeSkillVersion(v string) error {
	p, err := skillVersionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(v+"\n"), 0o644)
}

// ---------- ~/.claude/settings.json hook wiring ----------

func userSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func installSessionStartHook() (bool, error) {
	return installClaudeHook("SessionStart", hookMatcher, hookCommand)
}

func uninstallSessionStartHook() (bool, error) {
	return uninstallClaudeHook("SessionStart", hookCommand)
}

func uninstallUserPromptSubmitHook() (bool, error) {
	return uninstallClaudeHook("UserPromptSubmit", userPromptSubmitHookCommand)
}

// installClaudeHook idempotently adds a hook entry for the given Claude Code
// event to ~/.claude/settings.json, preserving all other keys/events/entries.
// Returns (added, err) where added is true iff the file was modified.
func installClaudeHook(event, matcher, command string) (bool, error) {
	path, err := userSettingsPath()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("read %s: %w", path, err)
		}
		raw = []byte("{}")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
		}
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	if settings == nil {
		settings = map[string]any{}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	entries, _ := hooks[event].([]any)

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
				return false, nil
			}
		}
	}

	newEntry := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": command,
			},
		},
	}
	if matcher != "" {
		newEntry["matcher"] = matcher
	}
	entries = append(entries, newEntry)
	hooks[event] = entries
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// uninstallClaudeHook removes any entry under hooks.<event> whose inner hook
// list contains a command matching the marker. Returns (removed, err).
func uninstallClaudeHook(event, command string) (bool, error) {
	path, err := userSettingsPath()
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false, nil
	}
	entries, _ := hooks[event].([]any)
	if len(entries) == 0 {
		return false, nil
	}

	changed := false
	kept := make([]any, 0, len(entries))
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			kept = append(kept, entry)
			continue
		}
		inner, _ := m["hooks"].([]any)
		filteredInner := make([]any, 0, len(inner))
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				filteredInner = append(filteredInner, h)
				continue
			}
			cmd, _ := hm["command"].(string)
			if strings.TrimSpace(cmd) == command {
				changed = true
				continue
			}
			filteredInner = append(filteredInner, h)
		}
		if len(filteredInner) == 0 {
			changed = true
			continue
		}
		m["hooks"] = filteredInner
		kept = append(kept, m)
	}

	if !changed {
		return false, nil
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}
