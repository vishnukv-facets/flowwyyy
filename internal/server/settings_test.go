package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestValidateKBDreamSchedule confirms the dreaming schedule setting reuses the
// playbook schedule validation (schedule.Parse): valid phrases/cron pass, blank
// passes (interval fallback), and gibberish is rejected instead of silently
// accepted.
func TestValidateKBDreamSchedule(t *testing.T) {
	sp, ok := settingSpecFor("FLOW_KB_DREAM_SCHEDULE")
	if !ok {
		t.Fatal("FLOW_KB_DREAM_SCHEDULE not registered")
	}
	for _, good := range []string{"", "daily at 3am", "every 6 hours", "weekly", "0 3 * * *"} {
		if err := validateSettingValue(sp, good); err != nil {
			t.Errorf("valid schedule %q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{"whenever i feel like it", "every blue moon", "garbage"} {
		if err := validateSettingValue(sp, bad); err == nil {
			t.Errorf("invalid schedule %q should be rejected", bad)
		}
	}
}

func TestUpdateSettings_PersistsAppliesAndMasksSecrets(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	// Contain env mutation to this test (t.Setenv restores originals on cleanup,
	// even though updateSettings calls os.Setenv directly).
	t.Setenv("FLOW_SLACK_TRIGGER_EMOJI", "")
	t.Setenv("FLOW_STALE_DAYS", "")
	t.Setenv("FLOW_SLACK_APP_TOKEN", "")

	resp, status := srv.updateSettings(actionRequest{Kind: "update-settings", Settings: map[string]string{
		"FLOW_SLACK_TRIGGER_EMOJI": "claude,codex",
		"FLOW_STALE_DAYS":          "5",
		"FLOW_SLACK_APP_TOKEN":     "xapp-secret-token",
	}})
	if status != 200 || !resp.OK {
		t.Fatalf("update resp=%+v status=%d", resp, status)
	}

	// Applied to the live process env.
	if got := os.Getenv("FLOW_SLACK_TRIGGER_EMOJI"); got != "claude,codex" {
		t.Fatalf("trigger emoji env = %q", got)
	}
	if got := os.Getenv("FLOW_STALE_DAYS"); got != "5" {
		t.Fatalf("stale days env = %q", got)
	}

	// Persisted to config.json.
	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_STALE_DAYS"] != "5" || cfg["FLOW_SLACK_APP_TOKEN"] != "xapp-secret-token" {
		t.Fatalf("config not persisted: %#v", cfg)
	}

	// GET masks the secret but surfaces non-secret values.
	rec := httptest.NewRecorder()
	srv.handleSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	body := rec.Body.String()
	if strings.Contains(body, "xapp-secret-token") {
		t.Fatalf("secret leaked in GET /api/settings: %s", body)
	}
	if !strings.Contains(body, "claude,codex") {
		t.Fatalf("non-secret value missing from GET: %s", body)
	}

	// Validation: a bad int is rejected.
	if _, st := srv.updateSettings(actionRequest{Settings: map[string]string{"FLOW_STALE_DAYS": "abc"}}); st == 200 {
		t.Fatal("expected validation error for non-integer FLOW_STALE_DAYS")
	}

	// Unknown key is rejected.
	if _, st := srv.updateSettings(actionRequest{Settings: map[string]string{"FLOW_BOGUS": "x"}}); st == 200 {
		t.Fatal("expected rejection of unknown setting key")
	}

	// Empty secret leaves the stored value unchanged.
	if _, st := srv.updateSettings(actionRequest{Settings: map[string]string{"FLOW_SLACK_APP_TOKEN": ""}}); st != 200 {
		t.Fatalf("empty-secret update should be a no-op success, got %d", st)
	}
	if loadConfigFile(srv.configPath())["FLOW_SLACK_APP_TOKEN"] != "xapp-secret-token" {
		t.Fatal("empty secret value clobbered the stored token")
	}
}

func TestSettingsExposeAutonomyPolicyForDedicatedPanel(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	t.Setenv("FLOW_STEERING_AUTONOMY", `{"make_task":{"enabled":true,"threshold":0.8}}`)
	t.Setenv("FLOW_SLACK_CLIENT_SECRET", "hidden-client-secret")

	rec := httptest.NewRecorder()
	srv.handleSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "FLOW_STEERING_AUTONOMY") || !strings.Contains(body, "make_task") {
		t.Fatalf("autonomy policy missing from GET /api/settings: %s", body)
	}
	if strings.Contains(body, "FLOW_SLACK_CLIENT_SECRET") || strings.Contains(body, "hidden-client-secret") {
		t.Fatalf("hidden Slack app secret surfaced in GET /api/settings: %s", body)
	}
}

// TestGitHubAppMetadataKeysHiddenAndRegistered proves the wizard-managed
// GitHub App metadata keys are registered under the git/github taxonomy, marked
// Hidden, and therefore never surface in the Settings form / /api/settings —
// the operator must not hand-edit credentials the Connect-GitHub wizard owns.
func TestGitHubAppMetadataKeysHiddenAndRegistered(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	appKeys := []string{
		"FLOW_GH_APP_ID",
		"FLOW_GH_APP_SLUG",
		"FLOW_GH_CLIENT_ID",
		"FLOW_GH_HTML_URL",
		"FLOW_GH_INSTALLATION_IDS",
	}
	for _, k := range appKeys {
		sp, ok := settingSpecFor(k)
		if !ok {
			t.Fatalf("%s not registered in settingsRegistry", k)
		}
		if !sp.Hidden {
			t.Errorf("%s should be Hidden (wizard-managed)", k)
		}
		if sp.Category != categoryGit || sp.Connector != connectorGitHub {
			t.Errorf("%s taxonomy = %q/%q, want git/github", k, sp.Category, sp.Connector)
		}
	}

	rec := httptest.NewRecorder()
	srv.handleSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	body := rec.Body.String()
	for _, k := range appKeys {
		if strings.Contains(body, k) {
			t.Errorf("hidden App metadata key %s leaked into /api/settings", k)
		}
	}
}

// TestSettingsExposeConnectorMetadata proves /api/settings carries the
// category/connector taxonomy the Connectors page groups by, that the values
// are stable for each provider, that non-connector settings (General) omit the
// taxonomy, that hidden Slack app credentials never leak, and that secrets stay
// masked. This is the contract the Connectors UI depends on — it must not
// regress to flat Group-only grouping.
func TestSettingsExposeConnectorMetadata(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	// A secret value present in the env must still be masked in the response.
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-should-stay-masked")

	rec := httptest.NewRecorder()
	srv.handleSettings(rec, httptest.NewRequest("GET", "/api/settings", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}

	var resp struct {
		Fields []struct {
			Key       string `json:"key"`
			Group     string `json:"group"`
			Category  string `json:"category"`
			Connector string `json:"connector"`
			Type      string `json:"type"`
			Value     string `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	byKey := map[string]struct{ category, connector, value, typ string }{}
	for _, f := range resp.Fields {
		byKey[f.Key] = struct{ category, connector, value, typ string }{f.Category, f.Connector, f.Value, f.Type}
	}

	wantTaxonomy := map[string][2]string{
		"FLOW_SLACK_TRIGGER_EMOJI": {"messaging", "slack"},
		"FLOW_SLACK_SELF_USER_IDS": {"messaging", "slack"},
		"FLOW_GH_ENABLED":          {"git", "github"},
		"FLOW_GH_SELF_LOGINS":      {"git", "github"},
		"FLOW_INGRESS_PROVIDER":    {"network", "ingress"},
		"FLOW_UI_KEEP_AWAKE":       {"network", "ingress"},
	}
	for key, want := range wantTaxonomy {
		got, ok := byKey[key]
		if !ok {
			t.Fatalf("%s missing from /api/settings", key)
		}
		if got.category != want[0] || got.connector != want[1] {
			t.Fatalf("%s taxonomy = %q/%q, want %q/%q", key, got.category, got.connector, want[0], want[1])
		}
	}

	// Non-connector settings carry no taxonomy (so Settings still owns them).
	if got := byKey["FLOW_STALE_DAYS"]; got.category != "" || got.connector != "" {
		t.Fatalf("FLOW_STALE_DAYS should have empty taxonomy, got %q/%q", got.category, got.connector)
	}

	// Hidden Slack app credentials must not appear at all.
	for _, k := range []string{"FLOW_SLACK_APP_ID", "FLOW_SLACK_CLIENT_ID", "FLOW_SLACK_CLIENT_SECRET"} {
		if _, ok := byKey[k]; ok {
			t.Fatalf("hidden credential %s leaked into /api/settings", k)
		}
	}

	// Secret values stay masked even when set in the env.
	if got := byKey["FLOW_SLACK_TOKEN"]; got.value != "" {
		t.Fatalf("secret FLOW_SLACK_TOKEN value should be masked, got %q", got.value)
	}
	if strings.Contains(rec.Body.String(), "xoxb-should-stay-masked") {
		t.Fatalf("secret leaked in /api/settings body")
	}
}

func TestSeedConfigFromEnv(t *testing.T) {
	root, db := testRootDB(t)
	// Config already pins one GitHub key; env disagrees — config must win and
	// stay untouched by seeding.
	if err := saveConfigFile(root+"/config.json", map[string]string{"FLOW_GH_AUTOOPEN": "true"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	// A non-secret operator setting that lives ONLY in the env (the real-world
	// case: FLOW_GH_* exported from a shell rc, never saved via the UI).
	t.Setenv("FLOW_GH_SELF_LOGINS", "octocat")
	t.Setenv("FLOW_GH_AUTOOPEN", "false")
	// A secret in the env must NOT be written to disk.
	t.Setenv("FLOW_SLACK_APP_TOKEN", "xapp-should-not-persist")

	New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"}) // boots applyConfigToEnv + seedConfigFromEnv

	cfg := loadConfigFile(root + "/config.json")
	if cfg["FLOW_GH_SELF_LOGINS"] != "octocat" {
		t.Fatalf("env-only non-secret not persisted: %#v", cfg)
	}
	if cfg["FLOW_GH_AUTOOPEN"] != "true" {
		t.Fatalf("seeding clobbered an existing config value: FLOW_GH_AUTOOPEN=%q, want true", cfg["FLOW_GH_AUTOOPEN"])
	}
	if _, ok := cfg["FLOW_SLACK_APP_TOKEN"]; ok {
		t.Fatalf("secret was persisted to disk by seeding: %#v", cfg)
	}
}

// TestApplySettingsRestart_SlackKeyResetsCommandCache verifies that
// applySettingsRestart resets the command channel cache when a Slack-related
// key changes, without panicking (no live listener in the test server).
func TestApplySettingsRestart_SlackKeyResetsCommandCache(t *testing.T) {
	root, db := testRootDB(t)
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	// s.slackListener is nil in the test server; the reset must still happen.
	slackKeys := []string{
		"FLOW_SLACK_SELF_USER_IDS",
		"FLOW_SLACK_TOKEN",
		"SLACK_BOT_TOKEN",
	}
	for _, k := range slackKeys {
		// Must not panic even with a nil slackListener.
		srv.applySettingsRestart([]string{k})
	}
	// Non-Slack key must also not panic.
	srv.applySettingsRestart([]string{"FLOW_STALE_DAYS"})
}

func TestApplyConfigToEnv_ConfigIsAuthoritative(t *testing.T) {
	root, db := testRootDB(t)
	if err := saveConfigFile(root+"/config.json", map[string]string{"FLOW_SLACK_TRIGGER_EMOJI": "fromconfig"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("FLOW_SLACK_TRIGGER_EMOJI", "fromenv")
	srv := New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
	_ = srv
	if got := os.Getenv("FLOW_SLACK_TRIGGER_EMOJI"); got != "fromconfig" {
		t.Fatalf("config should win at boot; env = %q", got)
	}
}
