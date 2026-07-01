package app

import (
	"encoding/json"
	"flow/internal/flowdb"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHookSessionStartUnboundEmitsAmbientHint pins the contract for
// ad-hoc sessions (no task carries the current $CLAUDE_CODE_SESSION_ID):
// the hook must emit a value-prop framing that names flow, instructs
// Skill-tool invocation, and explicitly disclaims any "substantive"
// gate. The skill — not the hook — owns the decision of whether to
// offer a task, save a KB entry, or stay quiet.
func TestHookSessionStartUnboundEmitsAmbientHint(t *testing.T) {
	setupFlowRoot(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	out := captureStdout(t, func() {
		if rc := cmdHookSessionStart(nil); rc != 0 {
			t.Fatalf("rc=%d", rc)
		}
	})
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse hook output: %v\nraw: %s", err, out)
	}
	if parsed.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", parsed.HookSpecificOutput.HookEventName)
	}
	ctx := parsed.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"already tracks",
		"`flow` skill",
		"Skill tool",
		"knowledge base",
		"AskUserQuestion",
		"existing flow task",
		"create a new one",
		// Hint substitutes the actual flowRoot() so paths reflect
		// $FLOW_ROOT (default ~/.flow). Match the suffix only.
		"/kb/ holds durable facts",
		"don't recognize",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("ambient hint missing %q; got:\n%s", want, ctx)
		}
	}
	// The hint must NOT mention "substantive" — naming the past gate
	// just primes Claude to think about gating again. Affirmative
	// framing only: load the skill, confirm task binding, proceed.
	if strings.Contains(ctx, "substantive") {
		t.Errorf("ambient hint must not mention 'substantive'; got:\n%s", ctx)
	}
	// Must NOT include task-specific instructions (no register-session,
	// no slug-bound reload).
	if strings.Contains(ctx, "flow register-session") {
		t.Errorf("ambient hint should not instruct register-session (no FLOW_TASK bound):\n%s", ctx)
	}
}

// TestHookSessionStartRequiresSkillInvocation pins the invariant that
// the injected additionalContext explicitly instructs the session to
// invoke the flow skill via the Skill tool as its first action, and
// mentions the task slug so the agent has something anchor-visible.
// The hook discovers the bound task by reverse-lookup against
// $CLAUDE_CODE_SESSION_ID (set by Claude Code in every real session)
// rather than by reading FLOW_TASK.
func TestHookSessionStartRequiresSkillInvocation(t *testing.T) {
	setupFlowRoot(t)

	// Seed a task and pin its session_id so the reverse-lookup finds it.
	seedTask(t, "some-slug")
	const sid = "deadbeef-1234-4567-8abc-def012345678"
	db := openFlowDB(t)
	if _, err := db.Exec(
		`UPDATE tasks SET session_id=?, status='in-progress', session_started=? WHERE slug='some-slug'`,
		sid, flowdb.NowISO(),
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)

	out := captureStdout(t, func() {
		if rc := cmdHookSessionStart(nil); rc != 0 {
			t.Fatalf("rc=%d", rc)
		}
	})

	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("parse hook output: %v\nraw: %s", err, out)
	}
	if parsed.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", parsed.HookSpecificOutput.HookEventName)
	}
	ctx := parsed.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "Skill tool") {
		t.Errorf("additionalContext must instruct Skill tool invocation, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "`flow` skill") {
		t.Errorf("additionalContext must name the `flow` skill, got:\n%s", ctx)
	}
	// Self-registration is gone — the UUID is pre-allocated by `flow do`.
	// Make sure we don't regress by re-introducing it here.
	if strings.Contains(ctx, "register-session") {
		t.Errorf("additionalContext should not mention register-session (pre-allocated by flow do):\n%s", ctx)
	}
	if !strings.Contains(ctx, "some-slug") {
		t.Errorf("additionalContext should mention the task slug, got:\n%s", ctx)
	}
}

// TestHookUserPromptSubmitIsNoOp pins the v0.1.0-alpha.7 contract:
// the UserPromptSubmit hook is a permanent no-op — exits 0 with no
// stdout regardless of session state. Kept around only for forward
// compatibility with stale settings.json entries on older installs.
// `flow skill install` actively removes the entry on upgrade.
func TestHookUserPromptSubmitIsNoOp(t *testing.T) {
	for _, sid := range []string{"", "deadbeef-1234-4567-8abc-def012345678"} {
		t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
		out := captureStdout(t, func() {
			if rc := cmdHookUserPromptSubmit(nil); rc != 0 {
				t.Fatalf("CLAUDE_CODE_SESSION_ID=%q: rc=%d", sid, rc)
			}
		})
		if strings.TrimSpace(out) != "" {
			t.Errorf("CLAUDE_CODE_SESSION_ID=%q: expected empty stdout, got:\n%s", sid, out)
		}
	}
}

func TestHookClaudeStatusLineCapturesUsageAndDelegates(t *testing.T) {
	root := setupFlowRoot(t)
	settingsPath, err := userSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	settings := readSettings(t, settingsPath)
	settings[claudeStatusLinePreviousKey] = map[string]any{
		"type":    "command",
		"command": "printf delegated-status",
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	input := `{
  "session_id": "abc123",
  "version": "2.1.90",
  "model": {"id": "claude-opus-4-8", "display_name": "Opus"},
  "effort": {"level": "xhigh"},
  "rate_limits": {
    "five_hour": {"used_percentage": 37, "resets_at": 1782397800},
    "seven_day": {"used_percentage": 67, "resets_at": 1782752400}
  },
  "workspace": {"current_dir": "/tmp/project"},
  "cost": {"total_cost_usd": 123.45}
}`
	stdout := withStdin(t, input, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if strings.TrimSpace(stdout) != "delegated-status" {
		t.Fatalf("stdout = %q, want delegated statusline output", stdout)
	}
	raw, err := os.ReadFile(filepath.Join(root, "provider_usage", "claude.json"))
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	var captured map[string]any
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatalf("parse capture: %v\n%s", err, raw)
	}
	rl, _ := captured["rate_limits"].(map[string]any)
	five, _ := rl["five_hour"].(map[string]any)
	if used, _ := five["used_percentage"].(float64); used != 37 {
		t.Fatalf("five_hour.used_percentage = %v, want 37", five["used_percentage"])
	}
	if _, ok := captured["cost"]; ok {
		t.Fatalf("capture should not persist raw cost payload: %#v", captured["cost"])
	}
	if _, ok := captured["workspace"]; ok {
		t.Fatalf("capture should not persist raw workspace payload: %#v", captured["workspace"])
	}
}

func TestHookClaudeStatusLineDoesNotClobberWithoutRateLimits(t *testing.T) {
	root := setupFlowRoot(t)
	path := filepath.Join(root, "provider_usage", "claude.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"rate_limits":{"five_hour":{"used_percentage":22,"resets_at":1782397800}}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	_ = withStdin(t, `{"model":{"id":"claude-opus-4-8"}}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("capture was clobbered by no-rate-limits payload:\n%s", got)
	}
}

// TestHookClaudeStatusLineRendersDefaultWithoutDelegate pins the fix for the
// blank-statusline regression: when no flowStatusLinePrevious command is
// configured (or it fails, e.g. it pointed at a script that got deleted),
// the hook must render its own statusline from the payload instead of
// printing nothing.
func TestHookClaudeStatusLineRendersDefaultWithoutDelegate(t *testing.T) {
	setupFlowRoot(t)

	fiveHourReset := time.Now().Add(3*time.Hour + 30*time.Minute).Unix()
	sevenDayReset := time.Now().Add(2*24*time.Hour + 4*time.Hour).Unix()
	input := fmt.Sprintf(`{
  "session_id": "abc123",
  "version": "2.1.90",
  "model": {"id": "claude-sonnet-5", "display_name": "Sonnet 5"},
  "effort": {"level": "xhigh"},
  "rate_limits": {
    "five_hour": {"used_percentage": 50, "resets_at": %d},
    "seven_day": {"used_percentage": 18, "resets_at": %d}
  },
  "context_window": {
    "context_window_size": 200000,
    "used_percentage": 33,
    "remaining_percentage": 67
  },
  "workspace": {"current_dir": "/tmp"},
  "cost": {"total_cost_usd": 1.23}
}`, fiveHourReset, sevenDayReset)
	stdout := withStdin(t, input, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		t.Fatal("expected a default statusline, got blank output")
	}
	if !strings.Contains(stdout, "Sonnet 5") {
		t.Errorf("statusline missing model name: %q", stdout)
	}
	if !strings.Contains(stdout, "xhigh") {
		t.Errorf("statusline missing effort level: %q", stdout)
	}
	if !strings.Contains(stdout, "$1.23") {
		t.Errorf("statusline missing cost: %q", stdout)
	}
	if !strings.Contains(stdout, "5h") || !strings.Contains(stdout, "50%") {
		t.Errorf("statusline missing 5h rate-limit bar at 50%%: %q", stdout)
	}
	if !strings.Contains(stdout, "7d") || !strings.Contains(stdout, "18%") {
		t.Errorf("statusline missing 7d rate-limit bar at 18%%: %q", stdout)
	}
	if !strings.Contains(stdout, "ctx") || !strings.Contains(stdout, "33%") {
		t.Errorf("statusline missing context-window fill bar at 33%%: %q", stdout)
	}
	if !strings.Contains(stdout, "200K") {
		t.Errorf("statusline missing context window size label: %q", stdout)
	}
	if !strings.Contains(stdout, "↻3h30m") {
		t.Errorf("statusline missing 5h reset countdown: %q", stdout)
	}
	if !strings.Contains(stdout, "↻2d4h") {
		t.Errorf("statusline missing 7d reset countdown: %q", stdout)
	}
}

// TestFormatResetCountdown pins the three display granularities: days once
// it's a day or more out, hours+minutes within a day, just minutes within
// the hour. Durations round to the nearest minute first so a few
// microseconds of test execution time can never flip a round number down
// (e.g. "2h5m" reading as "2h4m").
func TestFormatResetCountdown(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{2*time.Hour + 5*time.Minute, "2h5m"},
		{3*24*time.Hour + 4*time.Hour, "3d4h"},
		{45 * time.Minute, "45m"},
		{-time.Minute, "now"},
	}
	for _, c := range cases {
		if got := formatResetCountdown(time.Now().Add(c.in)); got != c.want {
			t.Errorf("formatResetCountdown(now+%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHookClaudeStatusLineContextWindowBeforeFirstMessage covers the
// used_percentage:null case (no API response yet) — the window size should
// still show even though there's nothing to put in a fill bar.
func TestHookClaudeStatusLineContextWindowBeforeFirstMessage(t *testing.T) {
	setupFlowRoot(t)
	stdout := withStdin(t, `{
  "model": {"display_name": "Sonnet 5"},
  "context_window": {"context_window_size": 1000000, "used_percentage": null}
}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if !strings.Contains(stdout, "1M") {
		t.Errorf("statusline missing 1M context window label: %q", stdout)
	}
	if strings.Contains(stdout, "%") {
		t.Errorf("statusline should not fabricate a fill percentage before the first message: %q", stdout)
	}
}

// TestHookClaudeStatusLineDefaultDisabled covers the escape hatch: an
// install that hits trouble with the built-in renderer can fully disable it
// via FLOW_STATUSLINE_DEFAULT and get the pre-existing blank-unless-delegate
// behavior back.
func TestHookClaudeStatusLineDefaultDisabled(t *testing.T) {
	setupFlowRoot(t)
	t.Setenv("FLOW_STATUSLINE_DEFAULT", "false")
	stdout := withStdin(t, `{"model":{"display_name":"Sonnet 5"},"cost":{"total_cost_usd":1.23}}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("FLOW_STATUSLINE_DEFAULT=false should suppress the built-in renderer, got %q", stdout)
	}
}

// TestHookClaudeStatusLineShowsBoundTaskName covers the other half of the
// default renderer: when the session_id in the payload is bound to a flow
// task, the task's name should appear in the statusline.
func TestHookClaudeStatusLineShowsBoundTaskName(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdAdd([]string{"task", "Skills cleanup", "--agent", "claude"}); rc != 0 {
		t.Fatalf("cmdAdd task: rc=%d", rc)
	}
	dbPath, err := flowDBPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET session_id = ? WHERE slug = ?`, "abc123", "skills-cleanup"); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	db.Close()

	stdout := withStdin(t, `{"session_id":"abc123","model":{"display_name":"Sonnet 5"}}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if !strings.Contains(stdout, "Skills cleanup") {
		t.Errorf("statusline missing bound task name: %q", stdout)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected task name on its own second line, got %d lines: %q", len(lines), stdout)
	}
	if !strings.Contains(lines[1], "Skills cleanup") {
		t.Errorf("task name should be on line 2, got line 1=%q line 2=%q", lines[0], lines[1])
	}
}

// TestHookClaudeStatusLineStrikesThroughDoneTask covers flow done's
// behavior of leaving session_id on the task row (so a reopen can resume
// the same conversation) — a closed task still shows in the statusline, but
// struck through rather than looking like a still-active one.
func TestHookClaudeStatusLineStrikesThroughDoneTask(t *testing.T) {
	setupFlowRoot(t)
	if rc := cmdAdd([]string{"task", "Skills cleanup", "--agent", "claude"}); rc != 0 {
		t.Fatalf("cmdAdd task: rc=%d", rc)
	}
	dbPath, err := flowDBPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET session_id = ?, status = 'done' WHERE slug = ?`, "abc123", "skills-cleanup"); err != nil {
		t.Fatalf("bind session and close: %v", err)
	}
	db.Close()

	stdout := withStdin(t, `{"session_id":"abc123","model":{"display_name":"Sonnet 5"}}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if !strings.Contains(stdout, ansiStrikethrough+"Skills cleanup"+ansiReset) {
		t.Errorf("done task name should be wrapped in strikethrough ANSI: %q", stdout)
	}
}

// TestHookClaudeStatusLineNetworkInfoOptIn covers the privacy-sensitive
// opt-in: FLOW_STATUSLINE_NETWORK_INFO defaults off (no IP/location/weather,
// no network calls), and only the cached snapshot — never a live fetch —
// feeds the render, so a render is never slowed down by it.
func TestHookClaudeStatusLineNetworkInfoOptIn(t *testing.T) {
	setupFlowRoot(t)
	oldSpawn := spawnNetworkStatusRefresh
	spawnNetworkStatusRefresh = func() {}
	t.Cleanup(func() { spawnNetworkStatusRefresh = oldSpawn })
	payload := `{"model":{"display_name":"Sonnet 5"}}`

	t.Run("disabled by default", func(t *testing.T) {
		stdout := withStdin(t, payload, func() string {
			return captureStdout(t, func() {
				if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
					t.Fatalf("rc=%d", rc)
				}
			})
		})
		if strings.Contains(stdout, "°C") {
			t.Errorf("network info should be off by default, got %q", stdout)
		}
	})

	t.Run("enabled but no cache yet renders nothing extra", func(t *testing.T) {
		t.Setenv("FLOW_STATUSLINE_NETWORK_INFO", "1")
		stdout := withStdin(t, payload, func() string {
			return captureStdout(t, func() {
				if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
					t.Fatalf("rc=%d", rc)
				}
			})
		})
		if strings.Contains(stdout, "°C") {
			t.Errorf("should not fabricate weather before any cache exists: %q", stdout)
		}
	})

	t.Run("enabled with a warm cache shows it", func(t *testing.T) {
		t.Setenv("FLOW_STATUSLINE_NETWORK_INFO", "1")
		cachePath, err := networkStatusCachePath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
			t.Fatal(err)
		}
		ns := networkStatus{IP: "203.0.113.7", City: "Bengaluru", Region: "Karnataka", WeatherTempC: 28, WeatherLabel: "Clear", FetchedAt: time.Now().UTC().Format(time.RFC3339), OK: true}
		data, err := json.Marshal(ns)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cachePath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		stdout := withStdin(t, payload, func() string {
			return captureStdout(t, func() {
				if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
					t.Fatalf("rc=%d", rc)
				}
			})
		})
		if !strings.Contains(stdout, "203.0.113.7") || !strings.Contains(stdout, "Bengaluru, Karnataka") || !strings.Contains(stdout, "28°C Clear") {
			t.Errorf("statusline missing cached network info: %q", stdout)
		}
	})
}

// TestHookClaudeStatusLinePrefersWorkingDelegate ensures a configured
// flowStatusLinePrevious command still wins over the built-in default —
// the fallback must not clobber a user's own working statusline script.
func TestHookClaudeStatusLinePrefersWorkingDelegate(t *testing.T) {
	setupFlowRoot(t)
	settingsPath, err := userSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	settings := readSettings(t, settingsPath)
	settings[claudeStatusLinePreviousKey] = map[string]any{
		"type":    "command",
		"command": "printf delegated-status",
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout := withStdin(t, `{"model":{"display_name":"Sonnet 5"}}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookClaudeStatusLine(nil); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if strings.TrimSpace(stdout) != "delegated-status" {
		t.Fatalf("stdout = %q, want delegated statusline output", stdout)
	}
}

func TestHookAgentEventSkipsAmbientCodexSession(t *testing.T) {
	t.Setenv("FLOW_HOOK_OWNED", "")
	called := false
	oldPost := agentHookPost
	agentHookPost = func(endpoint string, raw []byte, timeout time.Duration) error {
		called = true
		return nil
	}
	t.Cleanup(func() { agentHookPost = oldPost })

	out := withStdin(t, `{"hook_event_name":"PreToolUse","thread_id":"019e3c18-1149-7532-a1c0-31a4cfedb296"}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookAgentEvent([]string{"--provider", "codex", "--url", "http://127.0.0.1:1/hook"}); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if called {
		t.Fatal("ambient codex hook should not forward to the Flow UI")
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("ambient codex hook should emit no stdout/stderr, got %q", out)
	}
}

func TestHookAgentEventForwardsFlowOwnedCodexSession(t *testing.T) {
	t.Setenv("FLOW_HOOK_OWNED", "1")
	called := false
	oldPost := agentHookPost
	agentHookPost = func(endpoint string, raw []byte, timeout time.Duration) error {
		called = true
		if !strings.Contains(string(raw), `"flow_hook_owned":true`) {
			t.Fatalf("forwarded payload missing flow_hook_owned=true: %s", raw)
		}
		return nil
	}
	t.Cleanup(func() { agentHookPost = oldPost })

	_ = withStdin(t, `{"hook_event_name":"PreToolUse","thread_id":"019e3c18-1149-7532-a1c0-31a4cfedb296"}`, func() string {
		return captureStdout(t, func() {
			if rc := cmdHookAgentEvent([]string{"--provider", "codex", "--url", "http://127.0.0.1:1/hook"}); rc != 0 {
				t.Fatalf("rc=%d", rc)
			}
		})
	})
	if !called {
		t.Fatal("flow-owned codex hook should forward to the Flow UI")
	}
}

func withStdin(t *testing.T, input string, f func() string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdin
	os.Stdin = r
	done := make(chan struct{})
	go func() {
		_, _ = io.WriteString(w, input)
		_ = w.Close()
		close(done)
	}()
	out := f()
	<-done
	os.Stdin = old
	_ = r.Close()
	return out
}

// TestBuildBootstrapPromptInvokesSkill pins the same invariant for the
// fresh-spawn prompt used by `flow do` (the hook only covers resume).
func TestBuildBootstrapPromptInvokesSkill(t *testing.T) {
	prompt := buildBootstrapPrompt("task-x")
	if !strings.Contains(prompt, "flow skill") && !strings.Contains(prompt, "`flow` skill") {
		t.Errorf("bootstrap prompt must name the flow skill:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Skill tool") {
		t.Errorf("bootstrap prompt must instruct Skill tool invocation:\n%s", prompt)
	}
	if strings.Contains(prompt, "register-session") {
		t.Errorf("bootstrap prompt should not mention register-session (pre-allocated by flow do):\n%s", prompt)
	}
	if !strings.Contains(prompt, "task-x") {
		t.Errorf("bootstrap prompt must mention the task slug")
	}
}
