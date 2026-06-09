package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// GitHub "Connect" wizard backend — App-manifest flow.
//
// Unlike Slack (App Configuration Token → apps.manifest.create), GitHub's
// App-manifest flow needs no pre-existing credential:
//
//  1. create-app — flow builds an App manifest and hands it to the browser,
//     which auto-submits a form POST to github.com/settings/apps/new (or the
//     org variant). flow makes NO API call here; it only generates the state
//     nonce it will validate on the callback.
//  2. callback   — GitHub creates the App and redirects to redirect_url with a
//     single-use ?code=. flow exchanges it via
//     POST /app-manifests/{code}/conversions, which returns the app id, slug,
//     client id/secret, webhook secret, and PEM private key in one shot.
//  3. install    — flow links the operator to the App's install page; the
//     post-install redirect carries the installation_id.
//
// Secrets (PEM, client secret, webhook secret) go to the OS keyring; non-secret
// metadata goes to config.json (see persistGitHubApp).

// githubManifestEvents are the webhook events the App subscribes to — the exact
// set the dispatcher already normalizes (issues/PRs/comments/reviews).
var githubManifestEvents = []string{
	"issues",
	"issue_comment",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
}

// githubAppManifest builds the App-manifest document. webhookURL is the public
// ingress URL GitHub will sign deliveries to; redirectURL is where GitHub sends
// the conversion code. Built as a plain map (no template) so there is no
// escaping surface — the frontend marshals it into the auto-submitting form.
func githubAppManifest(name, webhookURL, redirectURL string) map[string]any {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "flow"
	}
	return map[string]any{
		"name": name,
		// Homepage URL is required by GitHub but never fetched; the public base
		// is a valid, stable choice.
		"url":          redirectURL,
		"redirect_url": redirectURL,
		// setup_url is where GitHub sends the operator after they install the
		// App — it carries ?installation_id=, which the callback captures so the
		// SDK can mint installation tokens with no manual entry.
		"setup_url":       redirectURL,
		"setup_on_update": true,
		// Public ("Any account") so the App can be installed on the operator's
		// personal account AND the orgs they admin. A private App installs only on
		// its owning account, which makes the personal+org "both" case impossible.
		// The App is unlisted (no marketplace), so "public" only widens *where it
		// can be installed*, not its discoverability.
		"public": true,
		"hook_attributes": map[string]any{
			"url":    webhookURL,
			"active": true,
		},
		"default_permissions": map[string]any{
			"issues":        "write",
			"pull_requests": "write",
			"metadata":      "read",
		},
		"default_events": githubManifestEvents,
	}
}

// githubManifestCreateURL is the github.com form action the browser POSTs the
// manifest to. Personal apps go to /settings/apps/new; org apps to
// /organizations/{org}/settings/apps/new. The state nonce rides as a query
// param and is echoed back on the redirect for CSRF protection.
func githubManifestCreateURL(target, org, state string) (string, error) {
	q := "?state=" + url.QueryEscape(state)
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "org", "organization":
		org = strings.TrimSpace(org)
		if org == "" {
			return "", errors.New("an organization name is required for an org install target")
		}
		return "https://github.com/organizations/" + url.PathEscape(org) + "/settings/apps/new" + q, nil
	default: // "user" / "personal" / ""
		return "https://github.com/settings/apps/new" + q, nil
	}
}

// githubManifestConversion is the credentials bundle returned by
// POST /app-manifests/{code}/conversions.
type githubManifestConversion struct {
	AppID         int64  `json:"id"`
	Slug          string `json:"slug"`
	NodeID        string `json:"node_id"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
}

// githubSetupAPI is a minimal client for the manifest-conversion endpoint.
// BaseURL honors FLOW_GH_API_BASE_URL so tests can point it at an httptest
// server.
type githubSetupAPI struct {
	BaseURL    string
	HTTPClient *http.Client
}

func newGitHubSetupAPI() *githubSetupAPI {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("FLOW_GH_API_BASE_URL")), "/")
	if base == "" {
		base = "https://api.github.com"
	}
	return &githubSetupAPI{BaseURL: base}
}

func (a *githubSetupAPI) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// convertManifest exchanges the single-use manifest code for App credentials.
// The endpoint needs no authentication — the code itself is the credential
// (single-use, ~1h TTL).
func (a *githubSetupAPI) convertManifest(ctx context.Context, code string) (githubManifestConversion, error) {
	var conv githubManifestConversion
	code = strings.TrimSpace(code)
	if code == "" {
		return conv, errors.New("missing manifest conversion code")
	}
	endpoint := a.BaseURL + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return conv, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.client().Do(req)
	if err != nil {
		return conv, fmt.Errorf("github manifest conversion: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return conv, fmt.Errorf("github manifest conversion: %s", githubAPIErrorMessage(data, resp.StatusCode))
	}
	if err := json.Unmarshal(data, &conv); err != nil {
		return conv, fmt.Errorf("github manifest conversion: unexpected response (HTTP %d)", resp.StatusCode)
	}
	if conv.AppID == 0 || conv.PEM == "" || conv.WebhookSecret == "" {
		return conv, errors.New("github returned an incomplete App credentials bundle")
	}
	return conv, nil
}

// githubAPIErrorMessage extracts the human-readable "message" from a GitHub
// error response body, falling back to the HTTP status when the body isn't the
// expected shape.
func githubAPIErrorMessage(body []byte, status int) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d", status)
}
