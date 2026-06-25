package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func cmdHookClaudeStatusLine(args []string) int {
	fs := flagSet("hook claude-statusline")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := io.ReadAll(os.Stdin)
	if err == nil && len(bytes.TrimSpace(raw)) > 0 {
		_ = writeClaudeStatusLineUsage(raw)
	}
	if out := runPreviousClaudeStatusLine(raw); out != "" {
		fmt.Print(out)
	}
	return 0
}

func writeClaudeStatusLineUsage(raw []byte) error {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	windows := sanitizedClaudeRateLimits(root["rate_limits"])
	if len(windows) == 0 {
		return nil
	}
	capture := map[string]any{
		"source":      "flow claude statusLine",
		"observed_at": time.Now().UTC().Format(time.RFC3339),
	}
	for _, key := range []string{"session_id", "version"} {
		if v, ok := root[key]; ok {
			capture[key] = v
		}
	}
	if model, ok := sanitizedStringMap(root["model"], "id", "display_name"); ok {
		capture["model"] = model
	}
	if effort, ok := sanitizedStringMap(root["effort"], "level"); ok {
		capture["effort"] = effort
	}
	capture["rate_limits"] = windows

	data, err := json.MarshalIndent(capture, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path, err := claudeUsageCapturePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func claudeUsageCapturePath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("FLOW_CLAUDE_USAGE_CAPTURE")); p != "" {
		return p, nil
	}
	root, err := flowRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "provider_usage", "claude.json"), nil
}

func sanitizedClaudeRateLimits(raw any) map[string]any {
	rl, _ := raw.(map[string]any)
	if rl == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"five_hour", "seven_day"} {
		obj, _ := rl[key].(map[string]any)
		if obj == nil {
			continue
		}
		win := map[string]any{}
		for _, field := range []string{"used_percentage", "resets_at"} {
			if v, ok := obj[field]; ok {
				win[field] = v
			}
		}
		if len(win) > 0 {
			out[key] = win
		}
	}
	return out
}

func sanitizedStringMap(raw any, keys ...string) (map[string]any, bool) {
	obj, _ := raw.(map[string]any)
	if obj == nil {
		return nil, false
	}
	out := map[string]any{}
	for _, key := range keys {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			out[key] = v
		}
	}
	return out, len(out) > 0
}

func runPreviousClaudeStatusLine(input []byte) string {
	command := previousClaudeStatusLineCommand()
	if command == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return out.String()
}

func previousClaudeStatusLineCommand() string {
	path, err := userSettingsPath()
	if err != nil {
		return ""
	}
	settings, err := readClaudeSettings(path)
	if err != nil {
		return ""
	}
	prev, _ := settings[claudeStatusLinePreviousKey].(map[string]any)
	if prev == nil {
		return ""
	}
	if typ, _ := prev["type"].(string); typ != "" && typ != "command" {
		return ""
	}
	command, _ := prev["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" || command == claudeStatusLineCommand {
		return ""
	}
	return command
}
