package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func clickUpSetupTestServer(t *testing.T) *Server {
	t.Helper()
	keyring.MockInit()
	for _, k := range []string{
		"FLOW_CLICKUP_CLIENT_ID",
		"FLOW_CLICKUP_CLIENT_SECRET",
		"FLOW_CLICKUP_ACCESS_TOKEN",
		"FLOW_CLICKUP_TEAM_ID",
		"FLOW_CLICKUP_TEAM_NAME",
		"FLOW_CLICKUP_WEBHOOK_ID",
		"FLOW_CLICKUP_WEBHOOK_SECRET",
		"FLOW_INGRESS_PROVIDER",
		"FLOW_PUBLIC_BASE_URL",
		"FLOW_CLICKUP_API_BASE_URL",
	} {
		os.Unsetenv(k)
		key := k
		t.Cleanup(func() { os.Unsetenv(key) })
	}
	root, db := testRootDB(t)
	t.Setenv("FLOW_INGRESS_PROVIDER", "manual")
	t.Setenv("FLOW_PUBLIC_BASE_URL", "https://flow.example.test")
	return New(Config{DB: db, FlowRoot: root, CommandPath: "/bin/false"})
}

func TestClickUpOAuthStartStoresCredentialsAndReturnsAuthorizeURL(t *testing.T) {
	srv := clickUpSetupTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/clickup/setup/oauth/start", strings.NewReader(`{"client_id":"cid","client_secret":"secret"}`))
	srv.handleClickUpSetupOAuthStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		AuthorizeURL string `json:"authorize_url"`
		RedirectURL  string `json:"redirect_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.RedirectURL != "https://flow.example.test"+clickUpOAuthCallbackPath {
		t.Fatalf("redirect_url = %q", out.RedirectURL)
	}
	u, err := url.Parse(out.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != clickUpAuthorizeBase {
		t.Fatalf("authorize base = %s", out.AuthorizeURL)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("redirect_uri") != out.RedirectURL || q.Get("state") == "" {
		t.Fatalf("authorize query = %v", q)
	}
	if got := loadConfigFile(srv.configPath())["FLOW_CLICKUP_CLIENT_ID"]; got != "cid" {
		t.Fatalf("client id not persisted: %q", got)
	}
	if got, _ := getClickUpSecret(keyringAcctClickUpClientSecret); got != "secret" {
		t.Fatalf("client secret not in keyring: %q", got)
	}
}

func TestClickUpPersonalTokenStoresTokenAndCapturesWorkspace(t *testing.T) {
	srv := clickUpSetupTestServer(t)
	t.Setenv("FLOW_CLICKUP_API_BASE_URL", "https://clickup.test/api/v2")
	useMockHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/team" {
			t.Errorf("unexpected ClickUp request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer personal-token" {
			t.Errorf("team auth = %q", got)
		}
		_, _ = w.Write([]byte(`{"teams":[{"id":"team-1","name":"Engineering"}]}`))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/clickup/setup/token", strings.NewReader(`{"token":"personal-token"}`))
	srv.handleClickUpSetupToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token status = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := getClickUpSecret(keyringAcctClickUpAccessToken); got != "personal-token" {
		t.Fatalf("access token not in keyring: %q", got)
	}
	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_CLICKUP_TEAM_ID"] != "team-1" || cfg["FLOW_CLICKUP_TEAM_NAME"] != "Engineering" {
		t.Fatalf("workspace not captured: %#v", cfg)
	}
}

func TestClickUpOAuthCallbackExchangesCodeAndCapturesWorkspace(t *testing.T) {
	srv := clickUpSetupTestServer(t)
	t.Setenv("FLOW_CLICKUP_API_BASE_URL", "https://clickup.test/api/v2")
	useMockHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s", r.Method)
			}
			body, _ := io.ReadAll(r.Body)
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			if req["client_id"] != "cid" || req["client_secret"] != "secret" || req["code"] != "the-code" {
				t.Errorf("token request = %#v", req)
			}
			_, _ = w.Write([]byte(`{"access_token":"clickup-token"}`))
		case "/api/v2/team":
			if got := r.Header.Get("Authorization"); got != "Bearer clickup-token" {
				t.Errorf("team auth = %q", got)
			}
			_, _ = w.Write([]byte(`{"teams":[{"id":"team-1","name":"Engineering"}]}`))
		default:
			t.Errorf("unexpected ClickUp request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	start := httptest.NewRecorder()
	srv.handleClickUpSetupOAuthStart(start, httptest.NewRequest(http.MethodPost, "/api/clickup/setup/oauth/start", strings.NewReader(`{"client_id":"cid","client_secret":"secret"}`)))
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", start.Code, start.Body.String())
	}
	state := srv.clickUpOAuth.state

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, clickUpOAuthCallbackPath+"?state="+state+"&code=the-code", nil)
	srv.handleClickUpSetupOAuthCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := getClickUpSecret(keyringAcctClickUpAccessToken); got != "clickup-token" {
		t.Fatalf("access token not in keyring: %q", got)
	}
	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_CLICKUP_TEAM_ID"] != "team-1" || cfg["FLOW_CLICKUP_TEAM_NAME"] != "Engineering" {
		t.Fatalf("workspace not captured: %#v", cfg)
	}
	if srv.clickUpOAuth != nil {
		t.Fatalf("pending OAuth was not cleared")
	}
}

func TestClickUpOAuthCallbackDoesNotGuessAcrossMultipleWorkspaces(t *testing.T) {
	srv := clickUpSetupTestServer(t)
	t.Setenv("FLOW_CLICKUP_API_BASE_URL", "https://clickup.test/api/v2")
	useMockHTTPTransport(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"clickup-token"}`))
		case "/api/v2/team":
			_, _ = w.Write([]byte(`{"teams":[{"id":"team-1","name":"Engineering"},{"id":"team-2","name":"Ops"}]}`))
		default:
			t.Errorf("unexpected ClickUp request path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	start := httptest.NewRecorder()
	srv.handleClickUpSetupOAuthStart(start, httptest.NewRequest(http.MethodPost, "/api/clickup/setup/oauth/start", strings.NewReader(`{"client_id":"cid","client_secret":"secret"}`)))
	if start.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", start.Code, start.Body.String())
	}
	state := srv.clickUpOAuth.state

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, clickUpOAuthCallbackPath+"?state="+state+"&code=the-code", nil)
	srv.handleClickUpSetupOAuthCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d: %s", rec.Code, rec.Body.String())
	}
	cfg := loadConfigFile(srv.configPath())
	if cfg["FLOW_CLICKUP_TEAM_ID"] != "" || cfg["FLOW_CLICKUP_TEAM_NAME"] != "" {
		t.Fatalf("workspace should not be guessed: %#v", cfg)
	}
}
