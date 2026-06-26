package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var clickUpHTTPClient = &http.Client{Timeout: 15 * time.Second}

const (
	clickUpDefaultAPIBase    = "https://api.clickup.com/api/v2"
	clickUpAuthorizeBase     = "https://app.clickup.com/api"
	clickUpOAuthCallbackPath = "/api/clickup/oauth/callback"
	clickUpOAuthTTL          = 15 * time.Minute
)

var clickUpWebhookEvents = []string{
	"taskCreated",
	"taskUpdated",
	"taskDeleted",
	"taskStatusUpdated",
	"taskAssigneeUpdated",
	"taskPriorityUpdated",
	"taskDueDateUpdated",
	"taskTagUpdated",
	"taskMoved",
	"taskCommentPosted",
	"taskCommentUpdated",
}

type ClickUpSetupStatusView struct {
	AccessTokenSet   bool   `json:"access_token_set"`
	ClientIDSet      bool   `json:"client_id_set"`
	ClientSecretSet  bool   `json:"client_secret_set"`
	RedirectURL      string `json:"redirect_url,omitempty"`
	OAuthActive      bool   `json:"oauth_active"`
	TeamID           string `json:"team_id,omitempty"`
	TeamName         string `json:"team_name,omitempty"`
	ListID           string `json:"list_id,omitempty"`
	WebhookURL       string `json:"webhook_url,omitempty"`
	WebhookID        string `json:"webhook_id,omitempty"`
	WebhookSecretSet bool   `json:"webhook_secret_set"`
	Registered       bool   `json:"registered"`
	Summary          string `json:"summary"`
}

func (s *Server) handleClickUpSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.clickUpSetupStatus())
}

func (s *Server) clickUpSetupStatus() ClickUpSetupStatusView {
	v := ClickUpSetupStatusView{
		AccessTokenSet:   strings.TrimSpace(os.Getenv("FLOW_CLICKUP_ACCESS_TOKEN")) != "",
		ClientIDSet:      strings.TrimSpace(os.Getenv("FLOW_CLICKUP_CLIENT_ID")) != "",
		ClientSecretSet:  strings.TrimSpace(os.Getenv("FLOW_CLICKUP_CLIENT_SECRET")) != "",
		RedirectURL:      s.connectorCallbackURL(clickUpOAuthCallbackPath),
		TeamID:           strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_ID")),
		TeamName:         strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_NAME")),
		ListID:           strings.TrimSpace(os.Getenv("FLOW_CLICKUP_LIST_ID")),
		WebhookURL:       s.connectorCallbackURL("/api/clickup/webhook"),
		WebhookID:        strings.TrimSpace(os.Getenv("FLOW_CLICKUP_WEBHOOK_ID")),
		WebhookSecretSet: clickUpWebhookSecret() != "",
	}
	s.clickUpSetupMu.Lock()
	v.OAuthActive = s.clickUpOAuth != nil && time.Now().Before(s.clickUpOAuth.expires)
	s.clickUpSetupMu.Unlock()
	v.Registered = v.WebhookID != ""
	v.Summary = clickUpSetupSummary(v)
	return v
}

func clickUpSetupSummary(v ClickUpSetupStatusView) string {
	if !v.AccessTokenSet {
		if v.OAuthActive {
			return "Finish authorizing ClickUp in the browser"
		}
		return "Connect to ClickUp"
	}
	if v.TeamID == "" {
		return "Choose a ClickUp Workspace ID"
	}
	if v.WebhookURL == "" {
		return "Start public ingress before registering the webhook"
	}
	if !v.Registered {
		return "Token and workspace ready — register the webhook"
	}
	return "Connected — receiving ClickUp webhooks"
}

type clickUpOAuthPending struct {
	state        string
	clientID     string
	clientSecret string
	redirectURL  string
	expires      time.Time
}

func clickUpAPIBase() string {
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("FLOW_CLICKUP_API_BASE_URL")), "/"); base != "" {
		return base
	}
	return clickUpDefaultAPIBase
}

func clickUpAuthorizeURL(clientID, redirectURL, state string) string {
	return clickUpAuthorizeBase + "?" + url.Values{
		"client_id":    {clientID},
		"redirect_uri": {redirectURL},
		"state":        {state},
	}.Encode()
}

func (s *Server) handleClickUpSetupOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	clientID := strings.TrimSpace(req.ClientID)
	if clientID == "" {
		clientID = strings.TrimSpace(os.Getenv("FLOW_CLICKUP_CLIENT_ID"))
	}
	clientSecret := strings.TrimSpace(req.ClientSecret)
	if clientSecret == "" {
		clientSecret = strings.TrimSpace(os.Getenv("FLOW_CLICKUP_CLIENT_SECRET"))
	}
	if clientID == "" || clientSecret == "" {
		writeError(w, fmt.Errorf("ClickUp OAuth client ID and secret are required"), http.StatusBadRequest)
		return
	}
	redirectURL := s.connectorCallbackURL(clickUpOAuthCallbackPath)
	if redirectURL == "" {
		writeError(w, fmt.Errorf("public ingress is required for the ClickUp OAuth callback"), http.StatusServiceUnavailable)
		return
	}
	state, err := randomState()
	if err != nil {
		writeError(w, fmt.Errorf("create OAuth state: %w", err), http.StatusInternalServerError)
		return
	}

	cfg := loadConfigFile(s.configPath())
	cfg["FLOW_CLICKUP_CLIENT_ID"] = clientID
	delete(cfg, "FLOW_CLICKUP_CLIENT_SECRET")
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		writeError(w, fmt.Errorf("save ClickUp client ID: %w", err), http.StatusInternalServerError)
		return
	}
	os.Setenv("FLOW_CLICKUP_CLIENT_ID", clientID)
	if err := storeClickUpSecret(keyringAcctClickUpClientSecret, clientSecret); err != nil {
		writeError(w, fmt.Errorf("store ClickUp client secret: %w", err), http.StatusInternalServerError)
		return
	}

	pending := &clickUpOAuthPending{
		state:        state,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirectURL,
		expires:      time.Now().Add(clickUpOAuthTTL),
	}
	s.clickUpSetupMu.Lock()
	s.clickUpOAuth = pending
	s.clickUpSetupMu.Unlock()
	s.publishUIChange("clickup-setup")
	writeJSON(w, map[string]any{
		"ok":            true,
		"authorize_url": clickUpAuthorizeURL(clientID, redirectURL, state),
		"redirect_url":  redirectURL,
	})
}

func (s *Server) handleClickUpSetupOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.clickUpSetupMu.Lock()
	pending := s.clickUpOAuth
	s.clickUpSetupMu.Unlock()
	if pending == nil {
		http.Error(w, "no ClickUp authorization in progress", http.StatusNotFound)
		return
	}

	fail := func(status int, public, internal string) {
		writeCallbackResultHTML(w, status, callbackError, "Couldn't connect ClickUp", htmlEscape(public))
		s.clickUpSetupMu.Lock()
		s.clickUpOAuth = nil
		s.clickUpSetupMu.Unlock()
		s.publishUIChange("clickup-setup")
		_ = internal
	}

	q := r.URL.Query()
	if time.Now().After(pending.expires) {
		fail(http.StatusGone, "this authorization link expired — start again from Mission Control", "ClickUp OAuth timed out")
		return
	}
	if q.Get("state") != pending.state {
		fail(http.StatusBadRequest, "state mismatch — start again from Mission Control", "ClickUp OAuth state mismatch")
		return
	}
	if errParam := q.Get("error"); errParam != "" {
		fail(http.StatusBadRequest, "ClickUp reported: "+errParam, "ClickUp authorize error: "+errParam)
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		fail(http.StatusBadRequest, "missing authorization code", "ClickUp callback carried no code")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, err := exchangeClickUpOAuth(ctx, pending.clientID, pending.clientSecret, code)
	if err != nil {
		fail(http.StatusBadGateway, "token exchange failed: "+err.Error(), err.Error())
		return
	}
	if err := storeClickUpSecret(keyringAcctClickUpAccessToken, token); err != nil {
		fail(http.StatusInternalServerError, "saving token failed: "+err.Error(), err.Error())
		return
	}
	_ = s.captureClickUpAuthorizedWorkspace(ctx, token)

	s.clickUpSetupMu.Lock()
	s.clickUpOAuth = nil
	s.clickUpSetupMu.Unlock()
	s.publishUIChange("clickup-setup")

	title := "ClickUp connected"
	if team := strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_NAME")); team != "" {
		title += " to " + team
	}
	writeCallbackResultHTML(w, http.StatusOK, callbackOK, title, "flow has the access token it needs.")
}

func (s *Server) handleClickUpSetupToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, fmt.Errorf("token is required"), http.StatusBadRequest)
		return
	}
	if err := storeClickUpSecret(keyringAcctClickUpAccessToken, token); err != nil {
		writeError(w, fmt.Errorf("store token: %w", err), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_ = s.captureClickUpAuthorizedWorkspace(ctx, token)
	s.clickUpSetupMu.Lock()
	s.clickUpOAuth = nil
	s.clickUpSetupMu.Unlock()
	s.publishUIChange("clickup-setup")
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleClickUpSetupRegisterWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	st := s.clickUpSetupStatus()
	if !st.AccessTokenSet {
		writeError(w, fmt.Errorf("ClickUp access token is not set"), http.StatusBadRequest)
		return
	}
	if st.TeamID == "" {
		writeError(w, fmt.Errorf("FLOW_CLICKUP_TEAM_ID is required"), http.StatusBadRequest)
		return
	}
	if st.WebhookURL == "" {
		writeError(w, fmt.Errorf("public ingress is not running"), http.StatusServiceUnavailable)
		return
	}
	id, secret, err := createClickUpWebhook(st.TeamID, st.ListID, st.WebhookURL, os.Getenv("FLOW_CLICKUP_ACCESS_TOKEN"))
	if err != nil {
		writeError(w, err, http.StatusBadGateway)
		return
	}
	if id == "" || secret == "" {
		writeError(w, fmt.Errorf("ClickUp did not return a webhook id and secret"), http.StatusBadGateway)
		return
	}
	cfg := loadConfigFile(s.configPath())
	cfg["FLOW_CLICKUP_WEBHOOK_ID"] = id
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		writeError(w, fmt.Errorf("save webhook id: %w", err), http.StatusInternalServerError)
		return
	}
	os.Setenv("FLOW_CLICKUP_WEBHOOK_ID", id)
	if err := storeClickUpSecret(keyringAcctClickUpWebhookSecret, secret); err != nil {
		writeError(w, fmt.Errorf("store webhook secret: %w", err), http.StatusInternalServerError)
		return
	}
	s.publishUIChange("clickup-setup")
	writeJSON(w, map[string]any{"ok": true, "webhook_id": id})
}

func (s *Server) handleClickUpSetupDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cfg := loadConfigFile(s.configPath())
	for _, key := range []string{
		"FLOW_CLICKUP_TEAM_NAME",
		"FLOW_CLICKUP_WEBHOOK_ID",
	} {
		delete(cfg, key)
		os.Unsetenv(key)
	}
	if err := saveConfigFile(s.configPath(), cfg); err != nil {
		writeError(w, fmt.Errorf("save settings: %w", err), http.StatusInternalServerError)
		return
	}
	_ = storeClickUpSecret(keyringAcctClickUpAccessToken, "")
	_ = storeClickUpSecret(keyringAcctClickUpClientSecret, "")
	_ = storeClickUpSecret(keyringAcctClickUpWebhookSecret, "")
	s.clickUpSetupMu.Lock()
	s.clickUpOAuth = nil
	s.clickUpSetupMu.Unlock()
	s.publishUIChange("clickup-setup")
	writeJSON(w, map[string]any{"ok": true})
}

func createClickUpWebhook(teamID, listID, endpoint, token string) (string, string, error) {
	reqBody := map[string]any{
		"endpoint": endpoint,
		"events":   clickUpWebhookEvents,
	}
	if strings.TrimSpace(listID) != "" {
		reqBody["list_id"] = strings.TrimSpace(listID)
	}
	body, _ := json.Marshal(reqBody)
	url := clickUpAPIBase() + "/team/" + strings.TrimSpace(teamID) + "/webhook"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", clickUpAuthorizationHeader(token))
	resp, err := clickUpHTTPClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("ClickUp create webhook: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		ID      string `json:"id"`
		Webhook struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		} `json:"webhook"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", fmt.Errorf("parse ClickUp create webhook response: %w", err)
	}
	id := strings.TrimSpace(out.ID)
	if id == "" {
		id = strings.TrimSpace(out.Webhook.ID)
	}
	return id, strings.TrimSpace(out.Webhook.Secret), nil
}

func exchangeClickUpOAuth(ctx context.Context, clientID, clientSecret, code string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     strings.TrimSpace(clientID),
		"client_secret": strings.TrimSpace(clientSecret),
		"code":          strings.TrimSpace(code),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, clickUpAPIBase()+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := clickUpHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ClickUp OAuth token: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("parse ClickUp OAuth token response: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return "", fmt.Errorf("ClickUp did not return an access token")
	}
	return strings.TrimSpace(out.AccessToken), nil
}

func (s *Server) captureClickUpAuthorizedWorkspace(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clickUpAPIBase()+"/team", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", clickUpAuthorizationHeader(token))
	resp, err := clickUpHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ClickUp authorized workspaces: %s", resp.Status)
	}
	var out struct {
		Teams []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"teams"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	if len(out.Teams) == 0 {
		return nil
	}
	cfg := loadConfigFile(s.configPath())
	currentTeamID := strings.TrimSpace(os.Getenv("FLOW_CLICKUP_TEAM_ID"))
	if currentTeamID == "" && len(out.Teams) != 1 {
		return nil
	}
	for _, team := range out.Teams {
		id := strings.TrimSpace(team.ID)
		name := strings.TrimSpace(team.Name)
		if id == "" {
			continue
		}
		if currentTeamID == "" || currentTeamID == id {
			cfg["FLOW_CLICKUP_TEAM_ID"] = id
			os.Setenv("FLOW_CLICKUP_TEAM_ID", id)
			if name != "" {
				cfg["FLOW_CLICKUP_TEAM_NAME"] = name
				os.Setenv("FLOW_CLICKUP_TEAM_NAME", name)
			}
			return saveConfigFile(s.configPath(), cfg)
		}
	}
	return nil
}

func clickUpAuthorizationHeader(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "pk_") || strings.HasPrefix(token, "pk-") {
		return token
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	return "Bearer " + token
}
