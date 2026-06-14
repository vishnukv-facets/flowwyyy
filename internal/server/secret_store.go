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

// loadSlackSecretsFromKeyring hydrates the process env from the Slack keyring
// service at boot.
func loadSlackSecretsFromKeyring() {
	loadKeyringSecrets(slackKeyringService, slackSecretAccounts)
}
