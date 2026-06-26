package server

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestMain installs the in-memory keyring mock for the whole server test
// binary. Server.New() hydrates the process env from the keyring at boot, so
// without this every test that constructs a Server would hit the real OS
// keychain (slow, and prompts on macOS).
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

// resetGitHubSecretsForTest gives a test a clean in-memory keyring and unsets
// the env vars the GitHub wizard hydrates/writes.
func resetGitHubSecretsForTest(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	for _, envKey := range githubSecretAccounts {
		os.Unsetenv(envKey)
		key := envKey
		t.Cleanup(func() { os.Unsetenv(key) })
	}
	for _, envKey := range append(githubAppConfigKeys, "FLOW_GH_SELF_LOGINS") {
		os.Unsetenv(envKey)
		key := envKey
		t.Cleanup(func() { os.Unsetenv(key) })
	}
}

func TestStoreGitHubSecret_RoundTripsThroughKeyringAndEnv(t *testing.T) {
	resetGitHubSecretsForTest(t)

	if err := storeGitHubSecret(keyringAcctWebhookSecret, "s3cr3t"); err != nil {
		t.Fatalf("storeGitHubSecret: %v", err)
	}

	// Persisted to keyring.
	got, err := getGitHubSecret(keyringAcctWebhookSecret)
	if err != nil {
		t.Fatalf("getGitHubSecret: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("keyring value = %q, want %q", got, "s3cr3t")
	}
	// And exported live to the env so the webhook handler sees it without a
	// keychain hit.
	if v := os.Getenv("FLOW_GH_WEBHOOK_SECRET"); v != "s3cr3t" {
		t.Errorf("FLOW_GH_WEBHOOK_SECRET = %q, want %q", v, "s3cr3t")
	}
	if v := githubWebhookSecret(); v != "s3cr3t" {
		t.Errorf("githubWebhookSecret() = %q, want %q", v, "s3cr3t")
	}
}

func TestStoreGitHubSecret_EmptyValueClears(t *testing.T) {
	resetGitHubSecretsForTest(t)
	if err := storeGitHubSecret(keyringAcctClientSecret, "abc"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := storeGitHubSecret(keyringAcctClientSecret, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got, err := getGitHubSecret(keyringAcctClientSecret)
	if err != nil {
		t.Fatalf("getGitHubSecret: %v", err)
	}
	if got != "" {
		t.Errorf("after clear keyring value = %q, want empty", got)
	}
	if v := os.Getenv("FLOW_GH_CLIENT_SECRET"); v != "" {
		t.Errorf("after clear env = %q, want empty", v)
	}
}

func TestGetGitHubSecret_AbsentReturnsEmptyNotError(t *testing.T) {
	resetGitHubSecretsForTest(t)
	got, err := getGitHubSecret(keyringAcctAppPEM)
	if err != nil {
		t.Fatalf("getGitHubSecret on absent: %v", err)
	}
	if got != "" {
		t.Errorf("absent secret = %q, want empty", got)
	}
}

func TestLoadGitHubSecretsFromKeyring_OverridesEnvWhenPresent(t *testing.T) {
	resetGitHubSecretsForTest(t)
	// A stale config/shell value is in the env.
	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "from-config")
	// The keyring (authoritative store) holds a different value.
	if err := setGitHubSecret(keyringAcctWebhookSecret, "from-keyring"); err != nil {
		t.Fatalf("setGitHubSecret: %v", err)
	}

	loadGitHubSecretsFromKeyring()

	if v := os.Getenv("FLOW_GH_WEBHOOK_SECRET"); v != "from-keyring" {
		t.Errorf("after load = %q, want keyring value to win", v)
	}
}

func TestLoadGitHubSecretsFromKeyring_AbsentPreservesEnvFallback(t *testing.T) {
	resetGitHubSecretsForTest(t)
	// No keyring entry; operator supplied the secret via the environment.
	t.Setenv("FLOW_GH_WEBHOOK_SECRET", "env-fallback")

	loadGitHubSecretsFromKeyring()

	if v := githubWebhookSecret(); v != "env-fallback" {
		t.Errorf("githubWebhookSecret() = %q, want env fallback preserved", v)
	}
}

// TestNew_HydratesGitHubSecretsFromKeyring proves the keyring → env hydration
// happens at boot, so the webhook handler reads the secret without a keychain
// hit per delivery.
func TestNew_HydratesGitHubSecretsFromKeyring(t *testing.T) {
	resetGitHubSecretsForTest(t)
	root, db := testRootDB(t)
	if err := setGitHubSecret(keyringAcctWebhookSecret, "boot-secret"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	_ = New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	if v := githubWebhookSecret(); v != "boot-secret" {
		t.Errorf("New() did not hydrate webhook secret from keyring: got %q", v)
	}
}

func resetClickUpSecretsForTest(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	for _, envKey := range clickUpSecretAccounts {
		os.Unsetenv(envKey)
		key := envKey
		t.Cleanup(func() { os.Unsetenv(key) })
	}
}

func TestStoreClickUpSecret_RoundTripsThroughKeyringAndEnv(t *testing.T) {
	resetClickUpSecretsForTest(t)

	if err := storeClickUpSecret(keyringAcctClickUpAccessToken, "cu-token"); err != nil {
		t.Fatalf("storeClickUpSecret: %v", err)
	}
	got, err := getClickUpSecret(keyringAcctClickUpAccessToken)
	if err != nil {
		t.Fatalf("getClickUpSecret: %v", err)
	}
	if got != "cu-token" {
		t.Fatalf("keyring value = %q, want cu-token", got)
	}
	if v := os.Getenv("FLOW_CLICKUP_ACCESS_TOKEN"); v != "cu-token" {
		t.Fatalf("FLOW_CLICKUP_ACCESS_TOKEN = %q, want cu-token", v)
	}
}

func TestNew_HydratesClickUpSecretsFromKeyring(t *testing.T) {
	resetClickUpSecretsForTest(t)
	root, db := testRootDB(t)
	if err := setClickUpSecret(keyringAcctClickUpWebhookSecret, "clickup-webhook-secret"); err != nil {
		t.Fatalf("seed keyring: %v", err)
	}

	_ = New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})

	if v := clickUpWebhookSecret(); v != "clickup-webhook-secret" {
		t.Errorf("New() did not hydrate ClickUp webhook secret from keyring: got %q", v)
	}
}
