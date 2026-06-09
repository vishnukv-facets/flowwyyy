package monitor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListGitHubAppInstallations_ReturnsPersonalAndOrg(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// One personal install + one org install — the "both" case.
		_, _ = w.Write([]byte(`[
			{"id": 11, "account": {"login": "vishnukv-facets", "type": "User"}},
			{"id": 22, "account": {"login": "Facets-cloud", "type": "Organization"}}
		]`))
	}))
	defer stub.Close()
	t.Setenv("FLOW_GH_API_BASE_URL", stub.URL)
	t.Setenv("FLOW_GH_APP_ID", "12345")
	t.Setenv("FLOW_GH_APP_PEM", testRSAKeyPEM(t))

	installs, ok, err := ListGitHubAppInstallations(context.Background())
	if err != nil || !ok {
		t.Fatalf("ListGitHubAppInstallations: ok=%v err=%v", ok, err)
	}
	if len(installs) != 2 {
		t.Fatalf("want 2 installations, got %d: %+v", len(installs), installs)
	}
	if installs[0].Account != "vishnukv-facets" || installs[0].Type != "User" {
		t.Errorf("install[0] = %+v, want personal User", installs[0])
	}
	if installs[1].Account != "Facets-cloud" || installs[1].Type != "Organization" {
		t.Errorf("install[1] = %+v, want org Organization", installs[1])
	}
}

func TestListGitHubAppInstallations_NoAppReturnsNotOK(t *testing.T) {
	t.Setenv("FLOW_GH_APP_ID", "")
	t.Setenv("FLOW_GH_APP_PEM", "")
	installs, ok, err := ListGitHubAppInstallations(context.Background())
	if err != nil || ok || installs != nil {
		t.Fatalf("want (nil,false,nil) when no App connected; got installs=%v ok=%v err=%v", installs, ok, err)
	}
}

func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

func TestFirstInstallationID(t *testing.T) {
	cases := map[string]int64{
		"456":         456,
		" 456 , 789 ": 456,
		",,789":       789,
		"":            0,
		"abc":         0,
	}
	for in, want := range cases {
		if got := firstInstallationID(in); got != want {
			t.Errorf("firstInstallationID(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestGitHubAppCredentials_RequiresAllParts(t *testing.T) {
	t.Setenv("FLOW_GH_APP_ID", "123")
	t.Setenv("FLOW_GH_APP_PEM", "PEM")
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "")
	if _, ok := gitHubAppCredentials(); ok {
		t.Error("missing installation id should yield ok=false")
	}
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "456")
	creds, ok := gitHubAppCredentials()
	if !ok || creds.AppID != 123 || creds.InstallationID != 456 || string(creds.PrivateKeyPEM) != "PEM" {
		t.Errorf("creds = %+v ok=%v", creds, ok)
	}
}

func TestNewGitHubInstallationClient_NoAppReturnsNotOK(t *testing.T) {
	t.Setenv("FLOW_GH_APP_ID", "")
	t.Setenv("FLOW_GH_APP_PEM", "")
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "")
	client, ok, err := newGitHubInstallationClient()
	if err != nil || ok || client != nil {
		t.Fatalf("want (nil,false,nil) when no App connected; got client=%v ok=%v err=%v", client, ok, err)
	}
}

func TestNewGitHubInstallationClient_MintsTokenAndAuthenticates(t *testing.T) {
	var sawAuth string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_minted","expires_at":"2099-01-01T00:00:00Z","permissions":{}}`))
			return
		}
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer stub.Close()

	t.Setenv("FLOW_GH_APP_ID", "123")
	t.Setenv("FLOW_GH_APP_PEM", testRSAKeyPEM(t))
	t.Setenv("FLOW_GH_INSTALLATION_IDS", "456")
	t.Setenv("FLOW_GH_API_BASE_URL", stub.URL)

	client, ok, err := newGitHubInstallationClient()
	if err != nil || !ok {
		t.Fatalf("build client: ok=%v err=%v", ok, err)
	}
	// Any authenticated call should trigger ghinstallation to mint + attach the
	// installation token against the stubbed base URL.
	if _, _, err := client.Issues.ListIssueTimeline(t.Context(), "o", "r", 5, nil); err != nil {
		t.Fatalf("ListIssueTimeline: %v", err)
	}
	if !strings.Contains(sawAuth, "ghs_minted") {
		t.Errorf("API call did not carry the minted installation token; Authorization=%q", sawAuth)
	}
}
