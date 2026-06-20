package flowbackup

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-github/v84/github"
)

// GitHub offsite backup uses the go-github SDK authenticated with a personal
// access token, NOT the gh CLI for API operations. The token is sourced from
// the environment (FLOW_BACKUP_TOKEN / GITHUB_TOKEN / GH_TOKEN) or, since gh is
// assumed present and authenticated, from `gh auth token`. flow's GitHub App
// connector is webhook-only and can't create a personal repo, so a user token
// is required to provision the private backup repo.
//
// PERSONAL ACCOUNT, NEVER AN ORG — load-bearing invariant. The backup repo holds
// the operator's KB (personal/org facts) and is provisioned under the token
// owner's *personal* account: EnsureGitHubRemote calls Repositories.Create with
// an EMPTY org argument (→ POST /user/repos, owned by the authenticated user)
// and refuses to proceed unless the token resolves to a personal User account.
// This is independent of which account(s) the GitHub App connector is installed
// on — installing the App on an org never routes backups to that org.

// ghToken returns the gh CLI's auth token. Used only as a TOKEN SOURCE (not for
// any GitHub API operation), so a gh-authenticated machine works out of the box.
var ghToken = func() string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	cmd := exec.Command("gh", "auth", "token")
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// backupGitHubToken resolves a GitHub token: env first, then the gh CLI.
func backupGitHubToken() string {
	if t := backupToken(); t != "" { // env (see remote.go)
		return t
	}
	return ghToken()
}

// githubClient builds a token-authenticated go-github client, or (nil, false)
// when no token is available. Overridable in tests (point BaseURL at a stub).
var githubClient = func() (*github.Client, bool) {
	tok := backupGitHubToken()
	if tok == "" {
		return nil, false
	}
	return github.NewClient(&http.Client{Timeout: 30 * time.Second}).WithAuthToken(tok), true
}

// GitHubLogin returns the authenticated personal GitHub login via the SDK, or
// "" when no token is available / the call fails.
func GitHubLogin() string {
	client, ok := githubClient()
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return ""
	}
	return u.GetLogin()
}

// GitHubBackupAvailable reports whether flow can use GitHub for offsite backup
// (a usable token resolves to an authenticated account).
func GitHubBackupAvailable() bool { return GitHubLogin() != "" }

// githubBackupRepoName is the private repo flow provisions for backups. Override
// with FLOW_BACKUP_GH_REPO.
func githubBackupRepoName() string {
	if v := strings.TrimSpace(os.Getenv("FLOW_BACKUP_GH_REPO")); v != "" {
		return v
	}
	return "flow-backup"
}

// EnsureGitHubRemote idempotently provisions a PRIVATE backup repo under the
// authenticated user's personal account (via the go-github SDK) and sets it as
// the offsite remote. Returns the remote URL and whether the repo was newly
// created. No-op ("", false, nil) when no token is available.
func EnsureGitHubRemote(root string) (url string, created bool, err error) {
	client, ok := githubClient()
	if !ok {
		return "", false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return "", false, fmt.Errorf("flowbackup: github whoami: %w", err)
	}
	login := u.GetLogin()
	// Defend the personal-account invariant. The empty org passed to Create
	// already targets the personal namespace structurally; this fails loud rather
	// than silently provisioning the KB backup under an unexpected identity:
	//   - no login → the token doesn't resolve to an account at all;
	//   - an explicit non-"User" type → an org or App/bot identity (GET /user
	//     returns "User" for a personal account, so anything else is wrong).
	// An empty type is tolerated (sparse response) since the empty-org Create is
	// the real guarantee.
	if login == "" {
		return "", false, fmt.Errorf("flowbackup: backup token does not resolve to a GitHub account — sign in with a personal token (e.g. `gh auth login` as yourself)")
	}
	if t := u.GetType(); t != "" && !strings.EqualFold(t, "User") {
		return "", false, fmt.Errorf("flowbackup: backup token belongs to a %q, not a personal user account (login %q) — flow backups live in your personal GitHub account, never an org", t, login)
	}
	repo := githubBackupRepoName()

	if _, resp, getErr := client.Repositories.Get(ctx, login, repo); getErr != nil {
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			return "", false, fmt.Errorf("flowbackup: check github backup repo: %w", getErr)
		}
		// 404 → create it private. org="" is REQUIRED here: it targets
		// POST /user/repos so the repo is owned by the personal account, never an
		// org. Do not pass an org argument.
		if _, _, createErr := client.Repositories.Create(ctx, "", &github.Repository{
			Name:        github.Ptr(repo),
			Private:     github.Ptr(true),
			Description: github.Ptr("flow backup — KB + briefs/updates (managed by flow; do not edit)"),
		}); createErr != nil {
			return "", false, fmt.Errorf("flowbackup: create private github backup repo: %w", createErr)
		}
		created = true
	}

	url = "https://github.com/" + login + "/" + repo + ".git"
	if err := SetRemote(root, url); err != nil {
		return "", created, err
	}
	return url, created, nil
}
