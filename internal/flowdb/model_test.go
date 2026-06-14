package flowdb

import "testing"

func TestMigrationAddsModelColumn(t *testing.T) {
	db := openTempDB(t)
	has, err := columnExists(db, "tasks", "model")
	if err != nil {
		t.Fatalf("columnExists(model): %v", err)
	}
	if !has {
		t.Error("tasks.model column should exist after migration")
	}
}

func TestTaskModelRoundTrip(t *testing.T) {
	db := openTempDB(t)
	now := NowISO()

	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, model, session_provider, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'medium', ?, ?, 'claude', ?, ?)`,
		"with-model", "Has a model", t.TempDir(), "opus", now, now,
	); err != nil {
		t.Fatalf("insert with-model: %v", err)
	}
	got, err := GetTask(db, "with-model")
	if err != nil {
		t.Fatalf("GetTask(with-model): %v", err)
	}
	if !got.Model.Valid || got.Model.String != "opus" {
		t.Errorf("Model = %+v, want valid opus", got.Model)
	}

	// A task with no explicit model stores NULL (resolution happens at launch).
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, session_provider, created_at, updated_at)
		 VALUES (?, ?, 'backlog', 'medium', ?, 'claude', ?, ?)`,
		"no-model", "No model", t.TempDir(), now, now,
	); err != nil {
		t.Fatalf("insert no-model: %v", err)
	}
	got2, err := GetTask(db, "no-model")
	if err != nil {
		t.Fatalf("GetTask(no-model): %v", err)
	}
	if got2.Model.Valid && got2.Model.String != "" {
		t.Errorf("Model = %+v, want NULL/empty for a task with no explicit model", got2.Model)
	}
}

func TestNormalizeModel(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"opus", "opus"},
		{"  sonnet  ", "sonnet"},
		{"gpt-5.4-mini", "gpt-5.4-mini"},
		{"claude-opus-4-8[1m]", "claude-opus-4-8[1m]"}, // passthrough preserves exact value
	}
	for _, c := range cases {
		if got := NormalizeModel(c.in); got != c.want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeModelTier(t *testing.T) {
	ok := map[string]string{
		"":        DefaultModelTier,
		"small":   "small",
		"MEDIUM":  "medium",
		" large ": "large",
	}
	for in, want := range ok {
		got, err := NormalizeModelTier(in)
		if err != nil {
			t.Errorf("NormalizeModelTier(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("NormalizeModelTier(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := NormalizeModelTier("huge"); err == nil {
		t.Error("NormalizeModelTier(\"huge\") expected error, got nil")
	}
}

func TestDownshiftTier(t *testing.T) {
	cases := map[string]string{
		"large":  "medium",
		"medium": "small",
		"small":  "small", // floor
	}
	for in, want := range cases {
		if got := DownshiftTier(in); got != want {
			t.Errorf("DownshiftTier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelForTier(t *testing.T) {
	cases := []struct {
		provider string
		tier     string
		want     string
	}{
		{"claude", "small", "haiku"},
		{"claude", "medium", "sonnet"},
		{"claude", "large", "opus"},
		{"codex", "small", "gpt-5.4-mini"},
		{"codex", "medium", "gpt-5.4"},
		{"codex", "large", "gpt-5.5"},
		{"", "medium", "sonnet"},      // unknown provider defaults to claude tiers
		{"claude", "bogus", "sonnet"}, // unknown tier falls back to medium
	}
	for _, c := range cases {
		if got := ModelForTier(c.provider, c.tier); got != c.want {
			t.Errorf("ModelForTier(%q, %q) = %q, want %q", c.provider, c.tier, got, c.want)
		}
	}
}

func TestModelTierFromEnv(t *testing.T) {
	t.Setenv("FLOW_MODEL_TIER", "")
	if got := ModelTierFromEnv(); got != DefaultModelTier {
		t.Errorf("unset FLOW_MODEL_TIER = %q, want %q", got, DefaultModelTier)
	}
	t.Setenv("FLOW_MODEL_TIER", "large")
	if got := ModelTierFromEnv(); got != "large" {
		t.Errorf("FLOW_MODEL_TIER=large = %q, want large", got)
	}
	t.Setenv("FLOW_MODEL_TIER", "nonsense")
	if got := ModelTierFromEnv(); got != DefaultModelTier {
		t.Errorf("invalid FLOW_MODEL_TIER should fall back to %q, got %q", DefaultModelTier, got)
	}
}

func TestAutoDownshiftEnabled(t *testing.T) {
	t.Setenv("FLOW_MODEL_AUTODOWNSHIFT", "")
	if !AutoDownshiftEnabled() {
		t.Error("unset FLOW_MODEL_AUTODOWNSHIFT should default to enabled")
	}
	for _, off := range []string{"off", "0", "false", "no", "OFF"} {
		t.Setenv("FLOW_MODEL_AUTODOWNSHIFT", off)
		if AutoDownshiftEnabled() {
			t.Errorf("FLOW_MODEL_AUTODOWNSHIFT=%q should disable downshift", off)
		}
	}
	t.Setenv("FLOW_MODEL_AUTODOWNSHIFT", "on")
	if !AutoDownshiftEnabled() {
		t.Error("FLOW_MODEL_AUTODOWNSHIFT=on should enable downshift")
	}
}

const descriptiveBrief = `# Add OAuth login

## What
Add OAuth login to the budgeting app so users can sign in with Google.

## Why
Users keep asking for single sign-on. Maintaining our own password store is a
liability and a support burden, and Google login is the most requested provider
by a wide margin across the last two quarters of feedback.

## Where
work_dir: /Users/me/code/budget

## Done when
- Users can sign in with Google from the login screen
- Sessions persist across browser restarts via secure cookies
- Existing password accounts can link a Google identity without losing data

## Out of scope
- Other providers (GitHub, Apple)
`

const thinBrief = `# Add OAuth login

## What
Add OAuth login to the budgeting app.

## Why
*Deferred — fill in at task start.*

## Where
work_dir: /Users/me/code/budget

## Done when
*Deferred — fill in at task start.*
`

const shortBrief = `# Tweak

## What
Fix it.

## Why
Because.

## Where
work_dir: /tmp/x

## Done when
- it works
- it passes
`

func TestBriefIsDescriptive(t *testing.T) {
	if !BriefIsDescriptive(descriptiveBrief) {
		t.Error("a fully-specified brief should be descriptive")
	}
	if BriefIsDescriptive(thinBrief) {
		t.Error("a brief with *Deferred* sections must not be descriptive")
	}
	if BriefIsDescriptive(shortBrief) {
		t.Error("a brief below the word threshold must not be descriptive")
	}
}

func TestResolveSessionModel(t *testing.T) {
	t.Setenv("FLOW_MODEL_TIER", "") // default medium
	t.Setenv("FLOW_MODEL_AUTODOWNSHIFT", "on")

	// Explicit choice always wins and is never adjusted (even at high priority).
	r := ResolveSessionModel("claude", "opus", descriptiveBrief, "high")
	if r.Model != "opus" || !r.Explicit || r.Downshifted || r.Upshifted {
		t.Errorf("explicit: got %+v, want Model=opus Explicit=true adjusted=false", r)
	}

	// Auto, non-descriptive brief -> default tier (medium -> sonnet).
	r = ResolveSessionModel("claude", "", shortBrief, "medium")
	if r.Model != "sonnet" || r.Explicit || r.Downshifted || r.Upshifted {
		t.Errorf("auto default: got %+v, want Model=sonnet Explicit=false adjusted=false", r)
	}

	// Auto, descriptive brief, non-high priority -> downshift medium -> small (haiku).
	r = ResolveSessionModel("claude", "", descriptiveBrief, "medium")
	if r.Model != "haiku" || r.Explicit || !r.Downshifted {
		t.Errorf("auto downshift: got %+v, want Model=haiku Explicit=false Downshifted=true", r)
	}

	// Codex auto downshift -> gpt-5.4-mini.
	r = ResolveSessionModel("codex", "", descriptiveBrief, "low")
	if r.Model != "gpt-5.4-mini" || !r.Downshifted {
		t.Errorf("codex downshift: got %+v, want Model=gpt-5.4-mini Downshifted=true", r)
	}

	// High priority upshifts the default tier (medium -> large -> opus) and is
	// never downshifted, even when the brief is descriptive.
	r = ResolveSessionModel("claude", "", descriptiveBrief, "high")
	if r.Model != "opus" || r.Explicit || !r.Upshifted || r.Downshifted {
		t.Errorf("high upshift: got %+v, want Model=opus Upshifted=true Downshifted=false", r)
	}

	// Codex high priority upshifts medium -> large -> gpt-5.5.
	r = ResolveSessionModel("codex", "", descriptiveBrief, "high")
	if r.Model != "gpt-5.5" || !r.Upshifted {
		t.Errorf("codex upshift: got %+v, want Model=gpt-5.5 Upshifted=true", r)
	}

	// Downshift disabled -> stays at default tier even for a descriptive brief.
	t.Setenv("FLOW_MODEL_AUTODOWNSHIFT", "off")
	r = ResolveSessionModel("claude", "", descriptiveBrief, "medium")
	if r.Model != "sonnet" || r.Downshifted {
		t.Errorf("downshift off: got %+v, want Model=sonnet Downshifted=false", r)
	}

	// Disabling downshift does not disable the high-priority upshift.
	r = ResolveSessionModel("claude", "", descriptiveBrief, "high")
	if r.Model != "opus" || !r.Upshifted {
		t.Errorf("downshift off + high: got %+v, want Model=opus Upshifted=true", r)
	}

	// Large default tier + descriptive brief -> downshift one step to medium.
	t.Setenv("FLOW_MODEL_AUTODOWNSHIFT", "on")
	t.Setenv("FLOW_MODEL_TIER", "large")
	r = ResolveSessionModel("claude", "", descriptiveBrief, "medium")
	if r.Model != "sonnet" || !r.Downshifted {
		t.Errorf("large+downshift: got %+v, want Model=sonnet Downshifted=true", r)
	}

	// Large default tier + high priority -> already at ceiling, no upshift.
	r = ResolveSessionModel("claude", "", descriptiveBrief, "high")
	if r.Model != "opus" || r.Upshifted || r.Downshifted {
		t.Errorf("large+high ceiling: got %+v, want Model=opus no adjustment", r)
	}
}
