package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/flowdb"

	_ "modernc.org/sqlite"
)

type providerUsageResponse struct {
	Provider          string                `json:"provider"`
	Available         bool                  `json:"available"`
	Limited           bool                  `json:"limited"`
	LimitReset        string                `json:"limit_reset_at,omitempty"`
	Reason            string                `json:"reason,omitempty"`
	Source            string                `json:"source,omitempty"`
	ObservedAt        string                `json:"observed_at,omitempty"`
	Windows           []providerUsageWindow `json:"windows"`
	QueuedActions     int                   `json:"queued_actions"`
	NextQueueRunAfter string                `json:"next_queue_run_after,omitempty"`
}

type providerUsageWindow struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	UsedPercent      int    `json:"used_percent"`
	RemainingPercent int    `json:"remaining_percent"`
	ResetAt          string `json:"reset_at,omitempty"`
	WindowMinutes    int    `json:"window_minutes,omitempty"`
}

func (s *Server) handleProviderUsage(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("provider")))
	if provider == "" {
		provider = "claude"
	}
	if provider != "claude" && provider != "codex" {
		writeError(w, fmt.Errorf("provider must be claude|codex"), http.StatusBadRequest)
		return
	}
	out := s.readProviderUsage(provider)
	writeJSON(w, out)
}

func (s *Server) readProviderUsage(provider string) providerUsageResponse {
	var out providerUsageResponse
	switch provider {
	case "codex":
		out = readCodexProviderUsage()
	default:
		out = readClaudeProviderUsage(s.cfg.FlowRoot)
	}
	return s.annotateProviderUsageQueue(out)
}

func (s *Server) annotateProviderUsageQueue(out providerUsageResponse) providerUsageResponse {
	if s == nil || s.cfg.DB == nil {
		return out
	}
	count, err := flowdb.CountPendingRateLimitQueue(s.cfg.DB)
	if err != nil {
		return out
	}
	next, ok, err := flowdb.NextRateLimitQueueRunAfter(s.cfg.DB)
	if err != nil || !ok {
		next = ""
	}
	out.QueuedActions = count
	out.NextQueueRunAfter = next
	return out
}

func readClaudeProviderUsage(flowRoot string) providerUsageResponse {
	for _, path := range claudeUsageCapturePaths(flowRoot) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		stale, observed := staleFile(path, 6*time.Hour)
		windows, err := claudeUsageWindows(data)
		if err != nil {
			return unavailableUsage("claude", err.Error())
		}
		if stale {
			return unavailableUsage("claude", "flow Claude usage capture is stale")
		}
		if len(windows) == 0 {
			return unavailableUsage("claude", "flow Claude usage capture has no rate_limits")
		}
		return annotateUsageLimit(providerUsageResponse{Provider: "claude", Available: true, Source: path, ObservedAt: observed, Windows: windows})
	}
	return unavailableUsage("claude", "flow Claude usage capture not found")
}

func claudeUsageCapturePaths(flowRoot string) []string {
	var paths []string
	if p := strings.TrimSpace(os.Getenv("FLOW_CLAUDE_USAGE_CAPTURE")); p != "" {
		paths = append(paths, p)
	}
	if strings.TrimSpace(flowRoot) != "" {
		paths = append(paths, filepath.Join(flowRoot, "provider_usage", "claude.json"))
	}
	return paths
}

func claudeUsageWindows(data []byte) ([]providerUsageWindow, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse flow Claude usage capture: %w", err)
	}
	rl, _ := root["rate_limits"].(map[string]any)
	if rl == nil {
		return nil, nil
	}
	specs := []struct {
		keys    []string
		id      string
		label   string
		minutes int
	}{
		{[]string{"five_hour", "primary"}, "five_hour", "5h", 300},
		{[]string{"seven_day", "secondary"}, "seven_day", "7d", 10080},
	}
	var out []providerUsageWindow
	for _, spec := range specs {
		for _, key := range spec.keys {
			obj, _ := rl[key].(map[string]any)
			if obj == nil {
				continue
			}
			win, ok := usageWindowFromMap(spec.id, spec.label, spec.minutes, obj)
			if ok {
				out = append(out, win)
				break
			}
		}
	}
	return out, nil
}

func readCodexProviderUsage() providerUsageResponse {
	path := strings.TrimSpace(os.Getenv("FLOW_CODEX_LOG_DB"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return unavailableUsage("codex", "home directory unavailable")
		}
		path = filepath.Join(home, ".codex", "logs_2.sqlite")
	}
	db, err := sql.Open("sqlite", sqliteReadOnlyDSN(path))
	if err != nil {
		return unavailableUsage("codex", err.Error())
	}
	defer db.Close()
	rows, err := db.Query(`SELECT feedback_log_body, ts FROM logs
		WHERE feedback_log_body LIKE '%websocket event: {"type":"codex.rate_limits"%'
		ORDER BY ts DESC, ts_nanos DESC, id DESC LIMIT 200`)
	if err != nil {
		return unavailableUsage("codex", err.Error())
	}
	defer rows.Close()
	var parseErr error
	var sawStale bool
	for rows.Next() {
		var body string
		var ts int64
		if err := rows.Scan(&body, &ts); err != nil {
			return unavailableUsage("codex", err.Error())
		}
		if time.Since(time.Unix(ts, 0)) > 6*time.Hour {
			sawStale = true
			continue
		}
		windows, topLevelLimited, err := codexUsageWindows(body)
		if errors.Is(err, errCodexRateLimitEventNotFound) {
			continue
		}
		if err != nil {
			parseErr = err
			continue
		}
		if len(windows) == 0 {
			parseErr = errors.New("codex rate limit event has no windows")
			continue
		}
		out := providerUsageResponse{
			Provider:   "codex",
			Available:  true,
			Limited:    topLevelLimited,
			Source:     path,
			ObservedAt: time.Unix(ts, 0).UTC().Format(time.RFC3339),
			Windows:    windows,
		}
		return annotateUsageLimit(out)
	}
	if err := rows.Err(); err != nil {
		return unavailableUsage("codex", err.Error())
	}
	if parseErr != nil {
		return unavailableUsage("codex", parseErr.Error())
	}
	if sawStale {
		return unavailableUsage("codex", "codex rate limit event is stale")
	}
	return unavailableUsage("codex", "codex rate limit event not found")
}

var errCodexRateLimitEventNotFound = errors.New("codex rate limit JSON not found")

func codexUsageWindows(body string) ([]providerUsageWindow, bool, error) {
	idx := strings.Index(body, "websocket event:")
	if idx < 0 {
		return nil, false, errCodexRateLimitEventNotFound
	}
	startRel := strings.Index(body[idx:], "{")
	if startRel < 0 {
		return nil, false, errCodexRateLimitEventNotFound
	}
	raw, ok := balancedJSONObject(body, idx+startRel)
	if !ok {
		return nil, false, errCodexRateLimitEventNotFound
	}
	var evt struct {
		Type       string         `json:"type"`
		RateLimits map[string]any `json:"rate_limits"`
	}
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		return nil, false, fmt.Errorf("parse codex rate limit event: %w", err)
	}
	if evt.Type != "codex.rate_limits" {
		return nil, false, errCodexRateLimitEventNotFound
	}
	specs := []struct {
		key     string
		id      string
		label   string
		minutes int
	}{
		{"primary", "five_hour", "5h", 300},
		{"secondary", "seven_day", "7d", 10080},
	}
	var out []providerUsageWindow
	limited, _ := evt.RateLimits["limit_reached"].(bool)
	if allowed, ok := evt.RateLimits["allowed"].(bool); ok && !allowed {
		limited = true
	}
	if typ, _ := evt.RateLimits["rate_limit_reached_type"].(string); strings.TrimSpace(typ) != "" {
		limited = true
	}
	for _, spec := range specs {
		if obj, _ := evt.RateLimits[spec.key].(map[string]any); obj != nil {
			win, ok := usageWindowFromMap(spec.id, spec.label, spec.minutes, obj)
			if ok {
				out = append(out, win)
			}
		}
	}
	return out, limited, nil
}

func balancedJSONObject(s string, start int) (string, bool) {
	if start < 0 || start >= len(s) || s[start] != '{' {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

func usageWindowFromMap(id, label string, fallbackMinutes int, obj map[string]any) (providerUsageWindow, bool) {
	used, ok := numberField(obj, "used_percentage", "used_percent")
	if !ok {
		if remaining, rok := numberField(obj, "percentage_remaining"); rok {
			used = 100 - remaining
			ok = true
		}
	}
	if !ok {
		return providerUsageWindow{}, false
	}
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	minutes := fallbackMinutes
	if m, ok := numberField(obj, "window_minutes"); ok && m > 0 {
		minutes = m
	}
	return providerUsageWindow{
		ID:               id,
		Label:            label,
		UsedPercent:      used,
		RemainingPercent: 100 - used,
		ResetAt:          resetAtField(obj, "resets_at", "reset_at"),
		WindowMinutes:    minutes,
	}, true
}

func numberField(obj map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		switch v := obj[key].(type) {
		case float64:
			return int(v + 0.5), true
		case int:
			return v, true
		}
	}
	return 0, false
}

func resetAtField(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		switch v := obj[key].(type) {
		case float64:
			if v > 0 {
				return time.Unix(int64(v), 0).UTC().Format(time.RFC3339)
			}
		case string:
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.UTC().Format(time.RFC3339)
			}
		}
	}
	return ""
}

func staleFile(path string, ttl time.Duration) (bool, string) {
	st, err := os.Stat(path)
	if err != nil {
		return false, ""
	}
	return time.Since(st.ModTime()) > ttl, st.ModTime().UTC().Format(time.RFC3339)
}

func sqliteReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	return u.String()
}

func unavailableUsage(provider, reason string) providerUsageResponse {
	return providerUsageResponse{Provider: provider, Available: false, Reason: reason, Windows: []providerUsageWindow{}}
}

func annotateUsageLimit(out providerUsageResponse) providerUsageResponse {
	if !out.Available {
		return out
	}
	if until, ok := providerUsageLimitedUntil(out, time.Now()); ok {
		out.Limited = true
		out.LimitReset = until.UTC().Format(time.RFC3339)
		return out
	}
	if out.Limited {
		if until, ok := providerUsageLatestReset(out, time.Now()); ok {
			out.LimitReset = until.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func providerUsageLimitedUntil(out providerUsageResponse, now time.Time) (time.Time, bool) {
	var until time.Time
	for _, win := range out.Windows {
		if win.UsedPercent < 100 && win.RemainingPercent > 0 {
			continue
		}
		reset, err := time.Parse(time.RFC3339, strings.TrimSpace(win.ResetAt))
		if err != nil || !reset.After(now) {
			continue
		}
		if until.IsZero() || reset.After(until) {
			until = reset
		}
	}
	return until, !until.IsZero()
}

func providerUsageLatestReset(out providerUsageResponse, now time.Time) (time.Time, bool) {
	var until time.Time
	for _, win := range out.Windows {
		reset, err := time.Parse(time.RFC3339, strings.TrimSpace(win.ResetAt))
		if err != nil || !reset.After(now) {
			continue
		}
		if until.IsZero() || reset.After(until) {
			until = reset
		}
	}
	return until, !until.IsZero()
}
