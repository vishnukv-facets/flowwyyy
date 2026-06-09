package monitor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v84/github"
)

// Native GitHub auth: a go-github client authenticated as the GitHub App
// installation. ghinstallation signs an App JWT with the private key (from the
// OS keyring, hydrated into FLOW_GH_APP_PEM at boot), exchanges it for a
// short-lived installation access token via
// POST /app/installations/{id}/access_tokens, and caches + refreshes that token
// transparently per request. This replaces the `gh` CLI as Flow's GitHub auth
// holder — no PAT, no keyring-backed OAuth token owned by `gh`.

// githubAppCredentials is the App identity needed to mint installation tokens.
type githubAppCredentials struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}

// gitHubAppCredentials reads the connected App's credentials from the process
// env (hydrated from config.json + the keyring at boot). ok is false until the
// App is fully connected AND installed (an installation id is required to mint
// tokens).
func gitHubAppCredentials() (githubAppCredentials, bool) {
	appID, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv("FLOW_GH_APP_ID")), 10, 64)
	pem := strings.TrimSpace(os.Getenv("FLOW_GH_APP_PEM"))
	instID := firstInstallationID(os.Getenv("FLOW_GH_INSTALLATION_IDS"))
	if appID == 0 || pem == "" || instID == 0 {
		return githubAppCredentials{}, false
	}
	return githubAppCredentials{AppID: appID, InstallationID: instID, PrivateKeyPEM: []byte(pem)}, true
}

// firstInstallationID returns the first valid id from a comma-separated list,
// or 0 when none. Flow mints tokens per installation; the first is the default
// target for single-installation setups (the common case).
func firstInstallationID(raw string) int64 {
	for _, p := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

// githubAPIBaseURL returns the API host override (GHES or tests). Same
// FLOW_GH_API_BASE_URL the setup wizard honors; empty means api.github.com.
func githubAPIBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("FLOW_GH_API_BASE_URL")), "/")
}

// newGitHubInstallationClient builds a go-github client authenticated as the
// App installation. ok is false (with nil error) when no App is connected yet —
// callers treat that as "GitHub not set up", not a failure.
func newGitHubInstallationClient() (*github.Client, bool, error) {
	creds, ok := gitHubAppCredentials()
	if !ok {
		return nil, false, nil
	}
	tr, err := ghinstallation.New(http.DefaultTransport, creds.AppID, creds.InstallationID, creds.PrivateKeyPEM)
	if err != nil {
		return nil, false, fmt.Errorf("github app installation transport: %w", err)
	}
	client := github.NewClient(&http.Client{Transport: tr, Timeout: 30 * time.Second})
	if base := githubAPIBaseURL(); base != "" {
		// ghinstallation mints tokens at tr.BaseURL; go-github sends API calls to
		// client.BaseURL. Point both at the override so GHES / tests work.
		tr.BaseURL = base
		if u, err := url.Parse(base + "/"); err == nil {
			client.BaseURL = u
		}
	}
	return client, true, nil
}

// newGitHubAppClient builds a go-github client authenticated as the App itself
// (a signed App JWT, not an installation token). App-level endpoints like
// GET /app/hook/deliveries require this. ok is false (nil error) when no App is
// connected — an installation id is NOT required here.
func newGitHubAppClient() (*github.Client, bool, error) {
	appID, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv("FLOW_GH_APP_ID")), 10, 64)
	pem := strings.TrimSpace(os.Getenv("FLOW_GH_APP_PEM"))
	if appID == 0 || pem == "" {
		return nil, false, nil
	}
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, appID, []byte(pem))
	if err != nil {
		return nil, false, fmt.Errorf("github app transport: %w", err)
	}
	client := github.NewClient(&http.Client{Transport: atr, Timeout: 30 * time.Second})
	if base := githubAPIBaseURL(); base != "" {
		atr.BaseURL = base
		if u, err := url.Parse(base + "/"); err == nil {
			client.BaseURL = u
		}
	}
	return client, true, nil
}

// GitHubInstallation is one account the connected App is installed on.
type GitHubInstallation struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	Type    string `json:"type"` // "User" | "Organization"
}

// ListGitHubAppInstallations returns every account the connected App is
// installed on — the operator's personal account plus any orgs they installed
// it on. It authenticates as the App itself (App JWT) and reads
// GET /app/installations, so it reflects GitHub's truth, not Flow's captured
// installation-id list. ok is false (nil error) when no App is connected yet.
func ListGitHubAppInstallations(ctx context.Context) (installs []GitHubInstallation, ok bool, err error) {
	client, ok, err := newGitHubAppClient()
	if err != nil || !ok {
		return nil, ok, err
	}
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.Apps.ListInstallations(ctx, opts)
		if err != nil {
			return nil, true, err
		}
		for _, in := range page {
			acct := in.GetAccount()
			installs = append(installs, GitHubInstallation{
				ID:      in.GetID(),
				Account: acct.GetLogin(),
				Type:    acct.GetType(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return installs, true, nil
}

// sdkOpenPRsForIssue returns the numbers of OPEN pull requests in the issue's
// own repo that are cross-referenced from the issue and authored by one of the
// self logins — the native-SDK replacement for the old `gh api .../timeline`
// shell-out. Returns (nil, nil) when no App is connected (nothing to link).
func sdkOpenPRsForIssue(ctx context.Context, owner, repo string, number int, selfLogins []string) ([]int, error) {
	client, ok, err := newGitHubInstallationClient()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	self := map[string]bool{}
	for _, l := range selfLogins {
		if v := strings.ToLower(strings.TrimSpace(l)); v != "" {
			self[v] = true
		}
	}
	sameRepo := strings.ToLower(owner + "/" + repo)
	seen := map[int]bool{}
	var prs []int
	opt := &github.ListOptions{PerPage: 100}
	for {
		events, resp, err := client.Issues.ListIssueTimeline(ctx, owner, repo, number, opt)
		if err != nil {
			return prs, err
		}
		for _, e := range events {
			if e.GetEvent() != "cross-referenced" {
				continue
			}
			src := e.GetSource()
			if src == nil || src.Issue == nil {
				continue
			}
			iss := src.Issue
			if iss.PullRequestLinks == nil {
				continue // cross-ref from a plain issue, not a PR
			}
			n := iss.GetNumber()
			if n <= 0 || seen[n] {
				continue
			}
			if !strings.EqualFold(iss.GetState(), "open") {
				continue
			}
			if iss.Repository != nil {
				if fn := strings.TrimSpace(iss.Repository.GetFullName()); fn != "" && !strings.EqualFold(fn, sameRepo) {
					continue
				}
			}
			if !self[strings.ToLower(strings.TrimSpace(iss.GetUser().GetLogin()))] {
				continue
			}
			seen[n] = true
			prs = append(prs, n)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return prs, nil
}
