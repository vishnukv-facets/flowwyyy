package app

import (
	"bytes"
	"context"
	"encoding/json"
	"flow/internal/flowdb"
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
		return 0
	}
	// No delegate configured (or it failed, e.g. the script it pointed at
	// was removed) — render flow's own statusline from the same payload
	// instead of leaving the bar blank. Gated by FLOW_STATUSLINE_DEFAULT so
	// an install that hits trouble with it (slow git/DB calls on an unusual
	// setup, a terminal that mishandles the ANSI color codes, etc.) can
	// disable it and fall back to the old blank-unless-delegate behavior.
	if statusLineDefaultEnabled() {
		if out := renderDefaultClaudeStatusLine(raw); out != "" {
			fmt.Print(out)
		}
	}
	return 0
}

// statusLineDefaultEnabled reports whether flow's built-in statusline
// renderer (renderDefaultClaudeStatusLine) may run. Default on; set
// FLOW_STATUSLINE_DEFAULT=0/false/off to disable it entirely — useful if it
// causes trouble for a particular setup (terminal, git, or DB access on the
// hot path of every render) and you'd rather have a blank statusline than a
// broken one.
func statusLineDefaultEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FLOW_STATUSLINE_DEFAULT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
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
	// Never chain back into flow's own statusLine command — that causes an
	// unbounded fork cascade. Use isClaudeStatusLineCommand (not a bare
	// string compare) so the FLOW_ROOT=...-prefixed form is also rejected.
	if command == "" || isClaudeStatusLineCommand(command) {
		return ""
	}
	return command
}

const (
	ansiReset         = "\x1b[0m"
	ansiDim           = "\x1b[2m"
	ansiGreen         = "\x1b[32m"
	ansiYellow        = "\x1b[33m"
	ansiRed           = "\x1b[31m"
	ansiStrikethrough = "\x1b[9m"
)

// renderDefaultClaudeStatusLine builds flow's own statusline segment from
// the same JSON payload Claude Code feeds the statusLine hook. It only runs
// when no delegate command produced output (see runPreviousClaudeStatusLine),
// so a removed or broken third-party statusline script degrades to this
// instead of a blank bar.
func renderDefaultClaudeStatusLine(raw []byte) string {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return ""
	}

	var segments []string

	if cwd := stringField(root["workspace"], "current_dir"); cwd != "" {
		label := filepath.Base(cwd)
		if branch := gitBranchFast(cwd); branch != "" {
			label = fmt.Sprintf("%s %s(%s)%s", label, ansiDim, branch, ansiReset)
		}
		segments = append(segments, label)
	}

	if model := firstNonEmpty(stringField(root["model"], "display_name"), stringField(root["model"], "id")); model != "" {
		if effort := stringField(root["effort"], "level"); effort != "" {
			model = fmt.Sprintf("%s %s%s%s", model, ansiDim, effort, ansiReset)
		}
		segments = append(segments, model)
	}

	if ctx := renderContextWindowSegment(root["context_window"]); ctx != "" {
		segments = append(segments, ctx)
	}

	if seg := renderRateLimitSegment(root["rate_limits"], "five_hour", "5h"); seg != "" {
		segments = append(segments, seg)
	}
	if seg := renderRateLimitSegment(root["rate_limits"], "seven_day", "7d"); seg != "" {
		segments = append(segments, seg)
	}

	if usd, ok := floatField(root["cost"], "total_cost_usd"); ok {
		segments = append(segments, fmt.Sprintf("$%.2f", usd))
	}

	// The task name and (opt-in) IP/location/weather info live on their own
	// line — keeps line one focused on the live session, line two on "where
	// and what for".
	line2 := renderNetworkStatusLine(currentStatusLineTaskLabel(stringField(root, "session_id")))

	if len(segments) == 0 && line2 == "" {
		return ""
	}
	var out strings.Builder
	if len(segments) > 0 {
		out.WriteString(strings.Join(segments, ansiDim+" · "+ansiReset))
	}
	if line2 != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(line2)
	}
	out.WriteString("\n")
	return out.String()
}

func stringField(obj any, key string) string {
	m, _ := obj.(map[string]any)
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func floatField(obj any, key string) (float64, bool) {
	m, _ := obj.(map[string]any)
	if m == nil {
		return 0, false
	}
	f, ok := m[key].(float64)
	return f, ok
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// renderContextWindowSegment reads the context_window object — the
// in-conversation token-fill metric, distinct from rate_limits (which is
// subscription-quota usage). context_window_size also tells us which model
// variant is active (e.g. the 200K vs 1M context beta), so we surface both:
// a fill bar when usage data exists yet, and the window size always.
func renderContextWindowSegment(raw any) string {
	cw, _ := raw.(map[string]any)
	if cw == nil {
		return ""
	}
	sizeLabel := ""
	if size, ok := cw["context_window_size"].(float64); ok && size > 0 {
		sizeLabel = formatContextWindowSize(size)
	}
	pct, hasPct := cw["used_percentage"].(float64)
	switch {
	case hasPct && sizeLabel != "":
		return fmt.Sprintf("ctx %s %s%s%s", renderUsageBar(pct), ansiDim, sizeLabel, ansiReset)
	case hasPct:
		return fmt.Sprintf("ctx %s", renderUsageBar(pct))
	case sizeLabel != "":
		// No messages sent yet (used_percentage is null) — window size is
		// still known and worth showing.
		return fmt.Sprintf("ctx %s%s%s", ansiDim, sizeLabel, ansiReset)
	default:
		return ""
	}
}

// formatContextWindowSize renders a raw token count as a short label, e.g.
// 200000 -> "200K", 1000000 -> "1M" — the two Claude context window tiers.
func formatContextWindowSize(tokens float64) string {
	switch {
	case tokens >= 1_000_000:
		mantissa := tokens / 1_000_000
		if mantissa == float64(int64(mantissa)) {
			return fmt.Sprintf("%dM", int64(mantissa))
		}
		return fmt.Sprintf("%.1fM", mantissa)
	case tokens >= 1000:
		return fmt.Sprintf("%.0fK", tokens/1000)
	default:
		return fmt.Sprintf("%.0f", tokens)
	}
}

// renderRateLimitSegment builds e.g. "5h [███░░░░░░░] 42% ↻3h12m" from
// rate_limits.<window> — Anthropic's own native quota-consumption figure for
// that rolling window, shown as-is (no rescaling: this is rate-limit usage,
// not in-conversation context-window fill).
func renderRateLimitSegment(raw any, window, label string) string {
	rl, _ := raw.(map[string]any)
	if rl == nil {
		return ""
	}
	win, _ := rl[window].(map[string]any)
	if win == nil {
		return ""
	}
	used, ok := win["used_percentage"].(float64)
	if !ok {
		return ""
	}
	used = min(max(used, 0), 100)
	seg := fmt.Sprintf("%s %s", label, renderUsageBar(used))
	if epoch, ok := win["resets_at"].(float64); ok {
		resetAt := time.Unix(int64(epoch), 0)
		seg += fmt.Sprintf(" %s↻%s%s", ansiDim, formatResetCountdown(resetAt), ansiReset)
	}
	return seg
}

// formatResetCountdown renders the time until t as a short label: "Xd Yh"
// once it's a day or more out, "Xh Ym" within a day, "Ym" within an hour.
func formatResetCountdown(t time.Time) string {
	d := time.Until(t)
	if d <= 0 {
		return "now"
	}
	totalMinutes := int(d.Round(time.Minute).Minutes())
	days := totalMinutes / (24 * 60)
	hours := (totalMinutes / 60) % 24
	minutes := totalMinutes % 60
	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

func contextUsageColor(pct float64) string {
	switch {
	case pct >= 85:
		return ansiRed
	case pct >= 60:
		return ansiYellow
	default:
		return ansiGreen
	}
}

// renderUsageBar draws a compact block-style progress bar (e.g.
// "[███░░░░░░] 42%"), color-coded by the same thresholds as the rest of
// the statusline.
func renderUsageBar(pct float64) string {
	const width = 10
	filled := min(max(int(pct/100*float64(width)), 0), width)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	color := contextUsageColor(pct)
	return fmt.Sprintf("%s[%s]%s %s%.0f%%%s", color, bar, ansiReset, color, pct, ansiReset)
}

// currentStatusLineTaskLabel resolves the flow task bound to this Claude
// session, if any. Best-effort: an unbound session (dispatch session, no
// flow root, no DB) silently yields "" rather than blocking the statusline.
func currentStatusLineTaskLabel(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	dbPath, err := flowDBPath()
	if err != nil {
		return ""
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		return ""
	}
	defer db.Close()
	task, err := flowdb.TaskBySessionID(db, sessionID)
	if err != nil || task == nil {
		return ""
	}
	name := firstNonEmpty(task.Name, task.Slug)
	const maxLen = 28
	if len(name) > maxLen {
		name = name[:maxLen-1] + "…"
	}
	if task.Status == "done" {
		// flow done keeps session_id on the task row so a reopen can resume
		// this exact conversation — so a closed task still shows here.
		// Strike it through rather than hiding it, so "done" stays visible
		// at a glance instead of looking like a still-active task.
		name = ansiStrikethrough + name + ansiReset
	}
	return name
}

// gitBranchFast best-effort resolves the current branch for the statusline.
// It's given a tight timeout because the statusline hook is on the hot path
// of every render — a slow or hung git call must not stall the UI.
func gitBranchFast(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}
