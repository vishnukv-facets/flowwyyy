package server

import (
	"errors"
	"os"

	"github.com/zalando/go-keyring"
)

// GitHub App credentials are stored at rest in the OS keyring rather than
// config.json. The PEM private key, OAuth client secret, and webhook signing
// secret never touch disk in plaintext; only non-secret App metadata (app id,
// slug, client id, installation ids) lives in config.json as Hidden settings.
const githubKeyringService = "flow.github"

const (
	keyringAcctWebhookSecret = "webhook_secret"
	keyringAcctAppPEM        = "app_pem"
	keyringAcctClientSecret  = "client_secret"
)

// githubSecretAccounts maps each keyring account to the process env var it
// hydrates. The rest of the code reads os.Getenv at call time (e.g.
// githubWebhookSecret), so loading the keyring into the env once at boot means
// no keychain access on the request hot path.
var githubSecretAccounts = map[string]string{
	keyringAcctWebhookSecret: "FLOW_GH_WEBHOOK_SECRET",
	keyringAcctAppPEM:        "FLOW_GH_APP_PEM",
	keyringAcctClientSecret:  "FLOW_GH_CLIENT_SECRET",
}

// keyringGet reads a secret from a keyring service, treating a missing entry as
// an empty value rather than an error.
func keyringGet(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// keyringDelete removes a secret from a keyring service, treating an absent
// entry as success.
func keyringDelete(service, account string) error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// storeKeyringSecret persists a secret to the keyring AND exports it to the
// process env so it takes effect live (handlers read env at call time). An empty
// value clears the secret from both. envKey may be "" for keyring-only entries.
func storeKeyringSecret(service, account, envKey, value string) error {
	if value == "" {
		if err := keyringDelete(service, account); err != nil {
			return err
		}
		if envKey != "" {
			os.Unsetenv(envKey)
		}
		return nil
	}
	if err := keyring.Set(service, account, value); err != nil {
		return err
	}
	if envKey != "" {
		os.Setenv(envKey, value)
	}
	return nil
}

// loadKeyringSecrets hydrates the process env from a keyring service at boot.
// Called after applyConfigToEnv, so a keyring-stored secret takes precedence
// over any config.json / shell value (the keyring is the authoritative at-rest
// store). An absent entry leaves the env untouched, preserving the back-compat
// env/config fallback.
func loadKeyringSecrets(service string, accounts map[string]string) {
	for account, envKey := range accounts {
		v, err := keyringGet(service, account)
		if err != nil || v == "" {
			continue
		}
		os.Setenv(envKey, v)
	}
}

// getGitHubSecret reads a GitHub App secret from the keyring.
func getGitHubSecret(account string) (string, error) {
	return keyringGet(githubKeyringService, account)
}

// setGitHubSecret writes a GitHub App secret to the keyring only.
func setGitHubSecret(account, value string) error {
	return keyring.Set(githubKeyringService, account, value)
}

// deleteGitHubSecret removes a GitHub App secret from the keyring.
func deleteGitHubSecret(account string) error {
	return keyringDelete(githubKeyringService, account)
}

// storeGitHubSecret persists a GitHub App secret to the keyring AND exports it
// to the process env so it takes effect live (the webhook handler and SDK read
// env). An empty value clears the secret from both.
func storeGitHubSecret(account, value string) error {
	return storeKeyringSecret(githubKeyringService, account, githubSecretAccounts[account], value)
}

// loadGitHubSecretsFromKeyring hydrates the process env from the GitHub keyring
// service at boot.
func loadGitHubSecretsFromKeyring() {
	loadKeyringSecrets(githubKeyringService, githubSecretAccounts)
}

// Slack secrets (bot token, operator user token, OAuth client secret) are
// stored at rest in the OS keyring rather than config.json, matching the GitHub
// App secret handling. Non-secret Slack metadata (app id, client id, self user
// ids, manifest rev) still lives in config.json.
const slackKeyringService = "flow.slack"

const (
	keyringAcctSlackBotToken     = "bot_token"
	keyringAcctSlackUserToken    = "user_token"
	keyringAcctSlackClientSecret = "client_secret"
)

// slackSecretAccounts maps each Slack keyring account to the process env var it
// hydrates. These three env keys are routed to the keyring by
// persistSlackSettings; everything else stays in config.json.
var slackSecretAccounts = map[string]string{
	keyringAcctSlackBotToken:     "FLOW_SLACK_TOKEN",
	keyringAcctSlackUserToken:    "FLOW_SLACK_USER_TOKEN",
	keyringAcctSlackClientSecret: "FLOW_SLACK_CLIENT_SECRET",
}

// slackSecretAccountForEnv reverse-maps a Slack env var to its keyring account,
// reporting whether the env var is a routed secret at all.
func slackSecretAccountForEnv(envKey string) (string, bool) {
	for account, env := range slackSecretAccounts {
		if env == envKey {
			return account, true
		}
	}
	return "", false
}

// storeSlackSecret persists a Slack secret to the keyring AND exports it to the
// process env. An empty value clears it from both.
func storeSlackSecret(account, value string) error {
	return storeKeyringSecret(slackKeyringService, account, slackSecretAccounts[account], value)
}

// secretKeyringRouteForEnv reports whether a setting/config env key is routed to
// the OS keyring (a Slack or GitHub secret), returning its keyring service and
// account. Used to keep keyring-backed secrets out of config.json on EVERY write
// path — the connect wizards, the generic Settings save (updateSettings), and the
// boot-time migration — so no path can leave a token in plaintext (audit P2-2).
func secretKeyringRouteForEnv(envKey string) (service, account string, ok bool) {
	if acct, found := slackSecretAccountForEnv(envKey); found {
		return slackKeyringService, acct, true
	}
	for acct, env := range githubSecretAccounts {
		if env == envKey {
			return githubKeyringService, acct, true
		}
	}
	if acct, found := clickUpSecretAccountForEnv(envKey); found {
		return clickUpKeyringService, acct, true
	}
	if envKey == backupSecretAccounts[keyringAcctBackupToken] {
		return backupKeyringService, keyringAcctBackupToken, true
	}
	return "", "", false
}

// loadSlackSecretsFromKeyring hydrates the process env from the Slack keyring
// service at boot.
func loadSlackSecretsFromKeyring() {
	loadKeyringSecrets(slackKeyringService, slackSecretAccounts)
}

// HydrateSlackSecretsFromKeyring loads Slack tokens from the OS keyring into
// the process env. Standalone CLI Slack reads call this because they may run in
// a fresh process with stale inherited env, while the keyring is authoritative.
func HydrateSlackSecretsFromKeyring() {
	loadSlackSecretsFromKeyring()
}

// ClickUp OAuth and webhook credentials are keyring-backed for the same reason
// as Slack/GitHub: access tokens and webhook signing secrets must not be written
// to config.json.
const clickUpKeyringService = "flow.clickup"

const (
	keyringAcctClickUpAccessToken   = "access_token"
	keyringAcctClickUpClientSecret  = "client_secret"
	keyringAcctClickUpWebhookSecret = "webhook_secret"
)

var clickUpSecretAccounts = map[string]string{
	keyringAcctClickUpAccessToken:   "FLOW_CLICKUP_ACCESS_TOKEN",
	keyringAcctClickUpClientSecret:  "FLOW_CLICKUP_CLIENT_SECRET",
	keyringAcctClickUpWebhookSecret: "FLOW_CLICKUP_WEBHOOK_SECRET",
}

func clickUpSecretAccountForEnv(envKey string) (string, bool) {
	for account, env := range clickUpSecretAccounts {
		if env == envKey {
			return account, true
		}
	}
	return "", false
}

func getClickUpSecret(account string) (string, error) {
	return keyringGet(clickUpKeyringService, account)
}

func setClickUpSecret(account, value string) error {
	return keyring.Set(clickUpKeyringService, account, value)
}

func storeClickUpSecret(account, value string) error {
	return storeKeyringSecret(clickUpKeyringService, account, clickUpSecretAccounts[account], value)
}

func loadClickUpSecretsFromKeyring() {
	loadKeyringSecrets(clickUpKeyringService, clickUpSecretAccounts)
}

// The offsite backup token (a personal GitHub PAT used to provision + push the
// private flow-backup repo) is stored at rest in the OS keyring, matching the
// GitHub App / Slack secret handling. It is the user's own credential — distinct
// from the App connector, which can't create a personal repo — so it gets its
// own namespace and is never written to config.json.
const backupKeyringService = "flow.backup"

const keyringAcctBackupToken = "token"

// backupSecretAccounts maps the backup keyring account to the env var it
// hydrates. flowbackup reads FLOW_BACKUP_TOKEN at call time.
var backupSecretAccounts = map[string]string{
	keyringAcctBackupToken: "FLOW_BACKUP_TOKEN",
}

// storeBackupSecret persists the offsite backup token to the keyring AND exports
// it to the process env so it takes effect live. An empty value clears both.
func storeBackupSecret(value string) error {
	return storeKeyringSecret(backupKeyringService, keyringAcctBackupToken, backupSecretAccounts[keyringAcctBackupToken], value)
}

// loadBackupSecretsFromKeyring hydrates FLOW_BACKUP_TOKEN from the backup keyring
// service at boot.
func loadBackupSecretsFromKeyring() {
	loadKeyringSecrets(backupKeyringService, backupSecretAccounts)
}
