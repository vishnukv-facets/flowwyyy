package flowbackup

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v84/github"
)

// stubGitHub points githubClient at an httptest server emulating the GitHub API
// endpoints EnsureGitHubRemote uses. createCalled (if non-nil) is set true when
// POST /user/repos is hit.
func stubGitHub(t *testing.T, repoExists bool, createCalled *bool) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": "octocat"})
	})
	mux.HandleFunc("/repos/octocat/flow-backup", func(w http.ResponseWriter, r *http.Request) {
		if repoExists {
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "flow-backup", "private": true})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body github.Repository
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.GetPrivate() {
			t.Errorf("repo create must request private=true; got %+v", body)
		}
		if createCalled != nil {
			*createCalled = true
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": "octocat/flow-backup"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL + "/")
	orig := githubClient
	githubClient = func() (*github.Client, bool) {
		c := github.NewClient(srv.Client())
		c.BaseURL = base
		return c, true
	}
	t.Cleanup(func() { githubClient = orig })
}

func TestEnsureGitHubRemoteCreatesWhenMissing(t *testing.T) {
	root := t.TempDir()
	var created bool
	stubGitHub(t, false, &created)

	url, wasCreated, err := EnsureGitHubRemote(root)
	if err != nil {
		t.Fatalf("EnsureGitHubRemote: %v", err)
	}
	if !wasCreated || !created {
		t.Fatalf("expected repo to be created (wasCreated=%v, POST hit=%v)", wasCreated, created)
	}
	want := "https://github.com/octocat/flow-backup.git"
	if url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}
	if RemoteURL(root) != want {
		t.Fatalf("remote not set to %q (got %q)", want, RemoteURL(root))
	}
}

func TestEnsureGitHubRemoteReusesExisting(t *testing.T) {
	root := t.TempDir()
	var created bool
	stubGitHub(t, true, &created)

	_, wasCreated, err := EnsureGitHubRemote(root)
	if err != nil {
		t.Fatalf("EnsureGitHubRemote: %v", err)
	}
	if wasCreated || created {
		t.Fatal("must NOT create when the repo already exists")
	}
	if RemoteURL(root) != "https://github.com/octocat/flow-backup.git" {
		t.Fatalf("remote should still be set to the existing repo")
	}
}

func TestGitHubBackupUnavailableWhenNoToken(t *testing.T) {
	root := t.TempDir()
	orig := githubClient
	githubClient = func() (*github.Client, bool) { return nil, false }
	t.Cleanup(func() { githubClient = orig })

	if GitHubBackupAvailable() {
		t.Fatal("expected GitHub backup unavailable with no token")
	}
	url, created, err := EnsureGitHubRemote(root)
	if err != nil || created || url != "" {
		t.Fatalf("expected no-op with no token; got url=%q created=%v err=%v", url, created, err)
	}
}
