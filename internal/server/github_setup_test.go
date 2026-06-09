package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestListGitHubOrgs_ParsesAndDedupes(t *testing.T) {
	origLook, origOrgs := ghLookPath, ghOrgsOutput
	t.Cleanup(func() { ghLookPath, ghOrgsOutput = origLook, origOrgs })
	ghLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	// gh api --jq emits one login per line; duplicates + blank lines can appear.
	ghOrgsOutput = func(context.Context) (string, error) { return "facets-cloud\nacme\nfacets-cloud\n\n", nil }

	orgs, err := listGitHubOrgs(context.Background())
	if err != nil {
		t.Fatalf("listGitHubOrgs: %v", err)
	}
	if len(orgs) != 2 || orgs[0] != "facets-cloud" || orgs[1] != "acme" {
		t.Fatalf("orgs = %#v, want [facets-cloud acme]", orgs)
	}
}

func TestListGitHubOrgs_GhMissing(t *testing.T) {
	origLook := ghLookPath
	t.Cleanup(func() { ghLookPath = origLook })
	ghLookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := listGitHubOrgs(context.Background()); err == nil {
		t.Fatalf("expected an error when gh is missing")
	}
}

func TestHandleGitHubSetupOrgs_ReturnsOrgsAndActiveLogin(t *testing.T) {
	origLook, origOrgs, origStatus := ghLookPath, ghOrgsOutput, ghAuthStatusOutput
	t.Cleanup(func() { ghLookPath, ghOrgsOutput, ghAuthStatusOutput = origLook, origOrgs, origStatus })
	ghLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	ghOrgsOutput = func(context.Context) (string, error) { return "facets-cloud\nacme\n", nil }
	ghAuthStatusOutput = func(context.Context) (string, error) {
		return "github.com\n  ✓ Logged in to github.com account vishnukv-facets (keyring)\n  - Active account: true\n", nil
	}

	rec := httptest.NewRecorder()
	(&Server{}).handleGitHubSetupOrgs(rec, httptest.NewRequest("GET", "/api/github/setup/orgs", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Orgs        []string `json:"orgs"`
		ActiveLogin string   `json:"active_login"`
		Error       string   `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Orgs) != 2 || resp.Orgs[0] != "facets-cloud" {
		t.Errorf("orgs = %#v", resp.Orgs)
	}
	if resp.ActiveLogin != "vishnukv-facets" {
		t.Errorf("active_login = %q", resp.ActiveLogin)
	}
	if resp.Error != "" {
		t.Errorf("unexpected error: %q", resp.Error)
	}
}

func TestHandleGitHubSetupOrgs_GhErrorDegradesGracefully(t *testing.T) {
	origLook, origOrgs := ghLookPath, ghOrgsOutput
	t.Cleanup(func() { ghLookPath, ghOrgsOutput = origLook, origOrgs })
	ghLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	ghOrgsOutput = func(context.Context) (string, error) { return "HTTP 401: Bad credentials", errors.New("exit status 1") }

	rec := httptest.NewRecorder()
	(&Server{}).handleGitHubSetupOrgs(rec, httptest.NewRequest("GET", "/api/github/setup/orgs", nil))
	// Still 200 so the wizard can fall back to manual entry, with an error string.
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (graceful)", rec.Code)
	}
	var resp struct {
		Orgs  []string `json:"orgs"`
		Error string   `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Orgs) != 0 {
		t.Errorf("orgs should be empty on error, got %#v", resp.Orgs)
	}
	if resp.Error == "" {
		t.Errorf("expected an error message")
	}
}

func TestHandleGitHubSetupInstallations_NoAppIsGraceful(t *testing.T) {
	// No App connected → empty list, 200, no error key (the monitor helper
	// returns ok=false,nil err; the happy path is covered in the monitor pkg).
	t.Setenv("FLOW_GH_APP_ID", "")
	t.Setenv("FLOW_GH_APP_PEM", "")

	rec := httptest.NewRecorder()
	(&Server{}).handleGitHubSetupInstallations(rec, httptest.NewRequest("GET", "/api/github/setup/installations", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Installations []map[string]any `json:"installations"`
		Error         string           `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Installations) != 0 || resp.Error != "" {
		t.Errorf("want empty installations and no error, got %+v", resp)
	}
}

func TestHandleGitHubSetupInstallations_RejectsPOST(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).handleGitHubSetupInstallations(rec, httptest.NewRequest("POST", "/api/github/setup/installations", nil))
	if rec.Code != 405 {
		t.Errorf("POST should be 405, got %d", rec.Code)
	}
}

func TestParseGitHubAuthStatus(t *testing.T) {
	t.Run("multi-source dedupes by login and detects env-pinned active", func(t *testing.T) {
		// Real gh 2.93 output: the active login appears twice (GH_TOKEN +
		// keyring) plus a second keyring-only login.
		out := `github.com
  ✓ Logged in to github.com account vishnukv-facets (GH_TOKEN)
  - Active account: true
  - Git operations protocol: ssh
  - Token: gho_****
  - Token scopes: 'repo'

  ✓ Logged in to github.com account vishnukv-facets (keyring)
  - Active account: false

  ✓ Logged in to github.com account vishnukv64 (keyring)
  - Active account: false
`
		accounts, active, source, host := parseGitHubAuthStatus(out)
		if active != "vishnukv-facets" {
			t.Fatalf("active = %q, want vishnukv-facets", active)
		}
		if source != "GH_TOKEN" {
			t.Fatalf("active source = %q, want GH_TOKEN", source)
		}
		if !isEnvTokenSource(source) {
			t.Fatalf("GH_TOKEN should be env-pinned")
		}
		if host != "github.com" {
			t.Fatalf("host = %q", host)
		}
		if len(accounts) != 2 {
			t.Fatalf("want 2 deduped accounts, got %d: %+v", len(accounts), accounts)
		}
		if !accounts[0].Active || accounts[0].Login != "vishnukv-facets" {
			t.Fatalf("first account should be active vishnukv-facets, got %+v", accounts[0])
		}
		if accounts[1].Login != "vishnukv64" || accounts[1].Active {
			t.Fatalf("second account should be inactive vishnukv64, got %+v", accounts[1])
		}
	})

	t.Run("keyring-only multi-account is switchable (not env-pinned)", func(t *testing.T) {
		out := `github.com
  ✓ Logged in to github.com account alice (keyring)
  - Active account: true

  ✓ Logged in to github.com account bob (keyring)
  - Active account: false
`
		accounts, active, source, _ := parseGitHubAuthStatus(out)
		if active != "alice" || source != "keyring" {
			t.Fatalf("active=%q source=%q, want alice/keyring", active, source)
		}
		if isEnvTokenSource(source) {
			t.Fatalf("keyring source must not be env-pinned")
		}
		if len(accounts) != 2 {
			t.Fatalf("want 2 accounts, got %d", len(accounts))
		}
	})

	t.Run("legacy single-account 'as' format is active", func(t *testing.T) {
		out := `github.com
  ✓ Logged in to github.com as octocat (oauth_token)
  - Git operations protocol: https
`
		accounts, active, _, _ := parseGitHubAuthStatus(out)
		if active != "octocat" {
			t.Fatalf("active = %q, want octocat", active)
		}
		if len(accounts) != 1 || !accounts[0].Active {
			t.Fatalf("want one active account, got %+v", accounts)
		}
	})

	t.Run("logged-out output yields no active account", func(t *testing.T) {
		out := "You are not logged into any GitHub hosts. To log in, run: gh auth login\n"
		accounts, active, _, _ := parseGitHubAuthStatus(out)
		if active != "" || len(accounts) != 0 {
			t.Fatalf("want no accounts, got active=%q accounts=%+v", active, accounts)
		}
	})
}
