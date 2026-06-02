package server

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

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
