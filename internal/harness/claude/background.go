package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"flow/internal/harness"
)

var BGCommandRunner = runBGCommand

func runBGCommand(workDir string, args []string) ([]byte, error) {
	if len(args) > 0 && args[0] == "agents" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "claude", args...)
		if workDir != "" {
			cmd.Dir = workDir
		}
		return cmd.CombinedOutput()
	}
	cmd := exec.Command("claude", args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	return cmd.CombinedOutput()
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")
var bannerShortIDRe = regexp.MustCompile(`backgrounded\s*·?\s*([0-9a-f]{8})\b`)

func parseBackgroundBanner(out string) (string, error) {
	clean := ansiRe.ReplaceAllString(out, "")
	firstLine := clean
	if i := strings.IndexByte(clean, '\n'); i >= 0 {
		firstLine = clean[:i]
	}
	m := bannerShortIDRe.FindStringSubmatch(firstLine)
	if m == nil {
		return "", fmt.Errorf("no background banner in output: %q", strings.TrimSpace(clean))
	}
	return m[1], nil
}

type bgAgentJSON struct {
	PID       int    `json:"pid"`
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	State     string `json:"state"`
}

func parseBackgroundAgents(raw []byte) ([]harness.BackgroundAgent, error) {
	var entries []bgAgentJSON
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse claude agents --json: %w", err)
	}
	out := make([]harness.BackgroundAgent, 0, len(entries))
	for _, e := range entries {
		if e.Kind != "background" {
			continue
		}
		out = append(out, harness.BackgroundAgent{
			ShortID:   e.ID,
			SessionID: e.SessionID,
			Name:      e.Name,
			Cwd:       e.Cwd,
			PID:       e.PID,
			Status:    e.Status,
			State:     e.State,
		})
	}
	return out, nil
}

func (c *claude) BackgroundAgents() ([]harness.BackgroundAgent, error) {
	out, err := BGCommandRunner("", []string{"agents", "--json", "--all"})
	if err != nil {
		return nil, fmt.Errorf("claude agents --json --all: %w", err)
	}
	return parseBackgroundAgents(out)
}

func (c *claude) launchAndCapture(workDir string, args []string, what string) (harness.BackgroundAgent, error) {
	out, err := BGCommandRunner(workDir, args)
	if err != nil {
		return harness.BackgroundAgent{}, fmt.Errorf("claude --bg (%s): %w (output: %s)", what, err, strings.TrimSpace(string(out)))
	}
	shortID, err := parseBackgroundBanner(string(out))
	if err != nil {
		return harness.BackgroundAgent{}, err
	}
	agents, err := c.BackgroundAgents()
	if err != nil {
		return harness.BackgroundAgent{}, fmt.Errorf("resolve session id for %s: %w", shortID, err)
	}
	for _, a := range agents {
		if a.ShortID == shortID {
			return a, nil
		}
	}
	return harness.BackgroundAgent{}, fmt.Errorf("backgrounded agent %s but it is not in `claude agents --json --all`", shortID)
}

func (c *claude) SpawnBackground(workDir, name, prompt string, opts harness.LaunchOpts) (harness.BackgroundAgent, error) {
	if opts.Inject != "" {
		prompt = prompt + "\n\n" + harness.InjectionMarker + "\n" + opts.Inject
	}
	args := []string{"--bg", "--name", name}
	args = append(args, claudeModelArgs(opts.Model)...)
	args = append(args, claudeEffortArgs(opts.Effort)...)
	args = append(args, claudePermissionArgs(opts.PermissionMode)...)
	args = append(args, prompt)
	return c.launchAndCapture(workDir, args, "spawn")
}

func (c *claude) ResumeBackground(workDir, sessionID string, opts harness.LaunchOpts) (harness.BackgroundAgent, error) {
	args := []string{"--bg", "--resume", sessionID}
	args = append(args, claudeModelArgs(opts.Model)...)
	args = append(args, claudeEffortArgs(opts.Effort)...)
	args = append(args, claudePermissionArgs(opts.PermissionMode)...)
	if opts.Inject != "" {
		args = append(args, harness.InjectionMarker+"\n"+opts.Inject)
	}
	return c.launchAndCapture(workDir, args, "resume")
}

func claudeModelArgs(model string) []string {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return []string{"--model", model}
}

func claudeEffortArgs(effort string) []string {
	if strings.TrimSpace(effort) == "" {
		return nil
	}
	return []string{"--effort", strings.TrimSpace(effort)}
}

func claudePermissionArgs(mode string) []string {
	switch strings.TrimSpace(mode) {
	case "auto":
		return []string{"--permission-mode", "auto"}
	case "bypass":
		return []string{"--dangerously-skip-permissions"}
	default:
		return []string{"--permission-mode", "acceptEdits"}
	}
}
