# Attention Router — P1.4a Backend API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

> ⚠️ **Repo rule:** work on branch `flow/attention-router-p1.1`; per-task commits pre-approved. Never commit to `main`.

**Goal:** Expose the Attention feed + steering settings to the Mission Control UI over the existing WS-RPC/REST surface: a `GET /api/attention` list endpoint, an `attention-act` action kind (make-task / forward / dismiss), a `GET /api/slack/channels` endpoint (for the channel multi-select), the steering settings registry entries, and a live-config read so settings changes take effect without restart.

**Architecture:** WS-RPC dispatches through the REST mux, so new `mux.HandleFunc("/api/...", ...)` routes are automatically RPC-callable. Read endpoints follow the `handleTasks`→`writeJSON(views)` pattern; mutations go through `POST /api/actions` (`runAction` switch) and publish a UI change automatically. Settings are registry-driven (`settingsRegistry` → config.json → `os.Setenv`). The steering cascade gains a `ConfigFn` so it reads watch-config live (settings changes apply without a listener restart). Slack channel listing adds a `GetConversationsContext` client behind a mockable seam.

**Tech Stack:** Go, `database/sql`, `github.com/slack-go/slack` v0.23.1, table-driven tests with `flowdb.OpenDB(t.TempDir()+"/flow.db")`, a minimal `&Server{cfg: Config{DB: db}}` (no full `New()` needed — `publishUIChange` nil-guards `s.events`), `httptest` for GET handlers, and function-var mock seams.

**Spec:** `docs/superpowers/specs/2026-06-04-attention-router-steerer-design.md` §7 (feed), §8 (actions), §10 (config/settings), §11 (UI surfaces).

**Builds on (this branch):** `flowdb.ListFeedItems`/`GetFeedItem`/`UpsertFeedItem`/`SetFeedItemStatus`/`FeedItem` (P1.1/P1.3b); `steering.ApplyAction`/`DismissFeed`/`ActionMakeTask`/`ActionForward`/`DefaultAutonomy`/`WatchConfigFromEnv`/`Cascade`/`NewCascade` (P1.2a/P1.3); server patterns: `actionRequest`/`actionResponse`/`runAction` (actions.go), `settingSpec`/`settingsRegistry`/`updateSettings` (settings.go), `registerAPIRoutes`/`writeJSON`/`getOnly`/`writeError`/`Config` (server.go/routes.go), `slackRepliesAPIClient`/`SlackBotToken` (monitor).

---

## File Structure

| File | Change |
|---|---|
| `internal/server/types.go` (modify) | Add `AttentionItemView` struct. |
| `internal/server/attention.go` (create) | `handleAttention` (GET list), `attentionItemView` builder, `attentionAct` (action handler), `handleSlackChannels`, `listSlackChannelsFn` var. |
| `internal/server/attention_test.go` (create) | View builder, `/api/attention` GET, `attention-act` dismiss/errors, `/api/slack/channels` (mocked). |
| `internal/server/actions.go` (modify) | Add `AttentionAction` field to `actionRequest`; add `case "attention-act"` to `runAction`. |
| `internal/server/server.go` (modify) | Register `/api/attention` + `/api/slack/channels` routes; set `cascade.ConfigFn` in the steering wiring. |
| `internal/server/settings.go` (modify) | Add `Steering` group registry entries (`FLOW_STEERING_WATCH_CHANNELS`, `_MUTED_CHANNELS`, `_MUTED_KEYWORDS`). |
| `internal/monitor/slack_channels.go` (create) | `SlackChannelInfo`, `ListSlackChannels`, mockable `slackConversationsFn`. |
| `internal/monitor/slack_channels_test.go` (create) | `ListSlackChannels` (mocked + no-token). |
| `internal/steering/cascade.go` (modify) | Add `ConfigFn func() WatchConfig`; use it in `Observe` when set. |
| `internal/steering/cascade_test.go` (modify) | Add a test that `ConfigFn` overrides the static `Config`. |

---

## Task 1: Feed read + act API

**Files:**
- Modify: `internal/server/types.go`, `internal/server/actions.go`, `internal/server/server.go`
- Create: `internal/server/attention.go`, `internal/server/attention_test.go`

- [ ] **Step 1: Write the failing test** — `internal/server/attention_test.go`

```go
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
)

func attentionTestServer(t *testing.T) (*Server, *flowdb.DB) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	t.Setenv("HOME", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Server{cfg: Config{DB: db, FlowRoot: root}}, db
}

func seedFeedItem(t *testing.T, db *flowdb.DB, id, status string) {
	t.Helper()
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: id, Source: "slack", ThreadKey: "C1:" + id, Summary: "s-" + id,
		SuggestedAction: "make_task", Confidence: 0.8, Status: status, CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestHandleAttention(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "a1", "new")
	seedFeedItem(t, db, "a2", "dismissed")

	req := httptest.NewRequest(http.MethodGet, "/api/attention?status=new", nil)
	rec := httptest.NewRecorder()
	s.handleAttention(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var views []AttentionItemView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(views) != 1 || views[0].ID != "a1" || views[0].SuggestedAction != "make_task" {
		t.Errorf("views = %+v, want only a1", views)
	}
}

func TestAttentionActDismiss(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "d1", "new")

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "d1", AttentionAction: "dismiss"})
	if status != 200 || !resp.OK {
		t.Fatalf("runAction = (%+v, %d), want OK 200", resp, status)
	}
	if items, _ := flowdb.ListFeedItems(db, "dismissed"); len(items) != 1 {
		t.Errorf("item should be dismissed, got %d dismissed", len(items))
	}
}

func TestAttentionActErrors(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "e1", "new")

	if _, status := s.runAction(actionRequest{Kind: "attention-act", AttentionAction: "dismiss"}); status != 400 {
		t.Errorf("missing target → %d, want 400", status)
	}
	if _, status := s.runAction(actionRequest{Kind: "attention-act", Target: "missing", AttentionAction: "dismiss"}); status != 404 {
		t.Errorf("missing item → %d, want 404", status)
	}
	if _, status := s.runAction(actionRequest{Kind: "attention-act", Target: "e1", AttentionAction: "frobnicate"}); status != 400 {
		t.Errorf("unknown action → %d, want 400", status)
	}
}

func TestAttentionItemView(t *testing.T) {
	v := attentionItemView(flowdb.FeedItem{ID: "x", Source: "slack", ThreadKey: "C1:1.1", Summary: "hi", SuggestedAction: "reply", Confidence: 0.5, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"})
	if v.ID != "x" || v.Source != "slack" || v.SuggestedAction != "reply" || v.Confidence != 0.5 {
		t.Errorf("view = %+v", v)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/server/ -run 'TestHandleAttention|TestAttentionAct|TestAttentionItemView' -v`
Expected: build failure — `undefined: AttentionItemView`, `handleAttention`, `attentionItemView`, `AttentionAction`.

- [ ] **Step 3: Add the view struct** — append to `internal/server/types.go`:

```go
// AttentionItemView is the UI shape of an attention_feed row.
type AttentionItemView struct {
	ID                string  `json:"id"`
	Source            string  `json:"source"`
	ThreadKey         string  `json:"thread_key"`
	Summary           string  `json:"summary"`
	SuggestedAction   string  `json:"suggested_action"`
	MatchedTask       string  `json:"matched_task,omitempty"`
	SuggestedProject  string  `json:"suggested_project,omitempty"`
	SuggestedPriority string  `json:"suggested_priority,omitempty"`
	Urgency           string  `json:"urgency,omitempty"`
	IsVIP             bool    `json:"is_vip"`
	Confidence        float64 `json:"confidence"`
	Draft             string  `json:"draft,omitempty"`
	Reason            string  `json:"reason,omitempty"`
	Status            string  `json:"status"`
	CreatedAt         string  `json:"created_at"`
	ActedAt           string  `json:"acted_at,omitempty"`
}
```

- [ ] **Step 4: Add the `AttentionAction` request field** — in `internal/server/actions.go`, add to the `actionRequest` struct (after the `Mkdir bool` field, before the `Settings` field):

```go
	// AttentionAction is the verb for the attention-act action kind:
	// make-task | forward | dismiss. Target carries the feed item id.
	AttentionAction string `json:"attention_action,omitempty"`
```

- [ ] **Step 5: Add the dispatch case** — in `runAction`'s `switch req.Kind`, add before the `default:` case:

```go
	case "attention-act":
		return s.attentionAct(req)
```

- [ ] **Step 6: Write the handlers** — `internal/server/attention.go`

```go
package server

import (
	"context"
	"net/http"
	"strings"

	"flow/internal/flowdb"
	"flow/internal/steering"
)

// handleAttention serves GET /api/attention[?status=new|acted|dismissed|all]
// (default: new). 'all' returns every row.
func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "new"
	}
	if status == "all" {
		status = ""
	}
	items, err := flowdb.ListFeedItems(s.cfg.DB, status)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	views := make([]AttentionItemView, 0, len(items))
	for _, it := range items {
		views = append(views, attentionItemView(it))
	}
	writeJSON(w, views)
}

func attentionItemView(it flowdb.FeedItem) AttentionItemView {
	return AttentionItemView{
		ID: it.ID, Source: it.Source, ThreadKey: it.ThreadKey, Summary: it.Summary,
		SuggestedAction: it.SuggestedAction, MatchedTask: it.MatchedTask,
		SuggestedProject: it.SuggestedProject, SuggestedPriority: it.SuggestedPriority,
		Urgency: it.Urgency, IsVIP: it.IsVIP, Confidence: it.Confidence,
		Draft: it.Draft, Reason: it.Reason, Status: it.Status,
		CreatedAt: it.CreatedAt, ActedAt: it.ActedAt,
	}
}

// attentionAct handles the attention-act action: make-task | forward | dismiss
// on a feed item (Target = feed id). Operator-initiated → manual=true bypasses
// the autonomy gate.
func (s *Server) attentionAct(req actionRequest) (actionResponse, int) {
	id := strings.TrimSpace(req.Target)
	if id == "" {
		return actionResponse{OK: false, Message: "attention-act requires a feed item id (target)"}, http.StatusBadRequest
	}
	item, err := flowdb.GetFeedItem(s.cfg.DB, id)
	if err != nil {
		return actionResponse{OK: false, Message: "feed item not found: " + id}, http.StatusNotFound
	}
	switch strings.ToLower(strings.TrimSpace(req.AttentionAction)) {
	case "dismiss":
		if err := steering.DismissFeed(s.cfg.DB, id); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "dismissed " + id}, http.StatusOK
	case "make-task", "make_task":
		if err := steering.ApplyAction(context.Background(), s.cfg.DB, item, steering.ActionMakeTask, steering.DefaultAutonomy(), true); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "made task from " + id}, http.StatusOK
	case "forward":
		if err := steering.ApplyAction(context.Background(), s.cfg.DB, item, steering.ActionForward, steering.DefaultAutonomy(), true); err != nil {
			return actionResponse{OK: false, Message: err.Error()}, http.StatusInternalServerError
		}
		return actionResponse{OK: true, Message: "forwarded " + id}, http.StatusOK
	default:
		return actionResponse{OK: false, Message: "unknown attention action: " + req.AttentionAction}, http.StatusBadRequest
	}
}
```

- [ ] **Step 7: Register the route** — in `registerAPIRoutes` (`server.go`), add alongside the other `/api/...` routes:

```go
	mux.HandleFunc("/api/attention", s.handleAttention)
```

- [ ] **Step 8: Run to verify it passes**

Run: `go test ./internal/server/ -run 'TestHandleAttention|TestAttentionAct|TestAttentionItemView' -v` → PASS (4).
Then `go build ./...`, `go vet ./internal/server/`, `gofmt -l internal/server/attention.go internal/server/types.go internal/server/actions.go` (no output).

- [ ] **Step 9: Commit**

```bash
git add internal/server/attention.go internal/server/attention_test.go internal/server/types.go internal/server/actions.go internal/server/server.go
git commit -m "feat(server): /api/attention list + attention-act action

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Steering settings + live config read

**Files:**
- Modify: `internal/server/settings.go`, `internal/steering/cascade.go`, `internal/server/server.go`
- Test: `internal/steering/cascade_test.go`

- [ ] **Step 1: Add registry entries** — in `internal/server/settings.go`, add to `settingsRegistry` (after the Slack group, as a new `Steering` group):

```go
	// Steering (attention router)
	{Key: "FLOW_STEERING_WATCH_CHANNELS", Label: "Watched channels", Group: "Steering", Type: settingString, Help: "Comma-separated Slack channel IDs the attention router watches (in addition to DMs + @mentions)."},
	{Key: "FLOW_STEERING_MUTED_CHANNELS", Label: "Muted channels", Group: "Steering", Type: settingString, Help: "Comma-separated Slack channel IDs to never surface."},
	{Key: "FLOW_STEERING_MUTED_KEYWORDS", Label: "Muted keywords", Group: "Steering", Type: settingString, Help: "Comma-separated keywords; messages containing them are dropped before triage."},
```

These persist to config.json and `os.Setenv` on save (via the existing `updateSettings`). `WatchConfigFromEnv()` reads them.

- [ ] **Step 2: Write the failing test** — append to `internal/steering/cascade_test.go`:

```go
func TestCascadeConfigFnOverridesStatic(t *testing.T) {
	c, _ := cascadeFixture(t) // static Config watches C1 (see cascadeFixture)
	// ConfigFn watches a DIFFERENT channel — proves Observe consults ConfigFn,
	// not the static Config captured at construction.
	c.ConfigFn = func() WatchConfig {
		return WatchConfig{WatchedChannels: map[string]bool{"C_LIVE": true}}
	}
	called := false
	stubClassifier(t, func(prompt string) (string, error) {
		called = true
		return `[{"thread_key":"C_LIVE:1.1","relevant":false}]`, nil // stage1 drops, cheap
	})
	// Message in C_LIVE (only in ConfigFn's set, NOT the static C1 set).
	if err := c.Observe(context.Background(), msg("C_LIVE", "1.1", "U_OTHER", "hi")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !called {
		t.Error("Stage 0 should have passed using ConfigFn's watched channels (classifier never ran)")
	}

	// And a message in the STATIC-only channel C1 must now drop (ConfigFn wins).
	called = false
	if err := c.Observe(context.Background(), msg("C1", "2.1", "U_OTHER", "hi")); err != nil {
		t.Fatalf("Observe C1: %v", err)
	}
	if called {
		t.Error("C1 is not in ConfigFn's set, so Stage 0 should drop it (classifier must not run)")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/steering/ -run TestCascadeConfigFnOverridesStatic -v`
Expected: failure — `c.ConfigFn undefined`.

- [ ] **Step 4: Add `ConfigFn` to the cascade** — in `internal/steering/cascade.go`:

Add the field to the `Cascade` struct (after `Config WatchConfig`):

```go
	// ConfigFn, when set, is called per Observe to read the watch-config live
	// (so Mission Control settings changes take effect without a restart). When
	// nil, the static Config is used. NewCascade leaves it nil; serve wiring
	// sets it to WatchConfigFromEnv.
	ConfigFn func() WatchConfig
```

Then at the very top of `Observe`, replace `s0 := Stage0(ev, c.Config)` with:

```go
	cfg := c.Config
	if c.ConfigFn != nil {
		cfg = c.ConfigFn()
	}
	s0 := Stage0(ev, cfg)
```

- [ ] **Step 5: Wire it in serve** — in `internal/server/server.go`, change the steering wiring (added in P1.2b) from:

```go
		dispatcher.Steerer = steering.NewCascade(cfg.DB, steering.WatchConfigFromEnv())
```

to:

```go
		cascade := steering.NewCascade(cfg.DB, steering.WatchConfigFromEnv())
		cascade.ConfigFn = steering.WatchConfigFromEnv // live re-read on settings changes
		dispatcher.Steerer = cascade
```

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/steering/ -run TestCascadeConfigFnOverridesStatic -v` → PASS.
Then `go test ./internal/steering/` (full package), `go build ./...`, `go vet ./internal/steering/ ./internal/server/`, `gofmt -l internal/steering/cascade.go internal/server/settings.go internal/server/server.go` (no output).

- [ ] **Step 7: Commit**

```bash
git add internal/server/settings.go internal/server/server.go internal/steering/cascade.go internal/steering/cascade_test.go
git commit -m "feat(steering): live watch-config (ConfigFn) + steering settings registry

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Slack channel-list endpoint

**Files:**
- Create: `internal/monitor/slack_channels.go`, `internal/monitor/slack_channels_test.go`
- Modify: `internal/server/attention.go` (add `handleSlackChannels` + `listSlackChannelsFn`), `internal/server/server.go` (route)
- Test: `internal/server/attention_test.go` (add channel-list handler test)

- [ ] **Step 1: Write the failing monitor test** — `internal/monitor/slack_channels_test.go`

```go
package monitor

import (
	"context"
	"errors"
	"testing"
)

func TestListSlackChannels(t *testing.T) {
	t.Setenv("FLOW_SLACK_TOKEN", "xoxb-test")
	old := slackConversationsFn
	slackConversationsFn = func(_ context.Context, token string) ([]SlackChannelInfo, error) {
		if token != "xoxb-test" {
			t.Fatalf("token = %q", token)
		}
		return []SlackChannelInfo{{ID: "C1", Name: "general", IsMember: true}}, nil
	}
	t.Cleanup(func() { slackConversationsFn = old })

	chans, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("ListSlackChannels: %v", err)
	}
	if len(chans) != 1 || chans[0].ID != "C1" || chans[0].Name != "general" {
		t.Errorf("chans = %+v", chans)
	}
}

func TestListSlackChannelsNoToken(t *testing.T) {
	t.Setenv("FLOW_SLACK_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_TOKEN", "")
	chans, err := ListSlackChannels(context.Background())
	if err != nil {
		t.Fatalf("no-token should be a graceful empty, got err %v", err)
	}
	if len(chans) != 0 {
		t.Errorf("no token → empty, got %d", len(chans))
	}
	_ = errors.New // keep import if unused elsewhere
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/monitor/ -run TestListSlackChannels -v`
Expected: build failure — `undefined: slackConversationsFn`, `SlackChannelInfo`, `ListSlackChannels`.

- [ ] **Step 3: Write the monitor implementation** — `internal/monitor/slack_channels.go`

```go
package monitor

import (
	"context"
	"strings"

	"github.com/slack-go/slack"
)

// SlackChannelInfo is the compact channel shape used by the channel picker.
type SlackChannelInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
	IsMember  bool   `json:"is_member"`
}

// slackConversationsFn is the mockable seam that hits conversations.list.
var slackConversationsFn = func(ctx context.Context, token string) ([]SlackChannelInfo, error) {
	api := slack.New(token)
	var out []SlackChannelInfo
	cursor := ""
	for {
		channels, next, err := api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types:           []string{"public_channel", "private_channel"},
			ExcludeArchived: true,
			Limit:           200,
			Cursor:          cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range channels {
			out = append(out, SlackChannelInfo{
				ID:        c.ID,
				Name:      c.Name,
				IsPrivate: c.IsPrivate,
				IsMember:  c.IsMember,
			})
		}
		if strings.TrimSpace(next) == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// ListSlackChannels returns the channels visible to the configured bot token.
// When no token is configured it returns an empty list (not an error) so the
// UI can render a "configure Slack" empty state gracefully.
func ListSlackChannels(ctx context.Context) ([]SlackChannelInfo, error) {
	token := SlackBotToken()
	if strings.TrimSpace(token) == "" {
		return nil, nil
	}
	return slackConversationsFn(ctx, token)
}
```

> **Field-access note:** `c.ID`/`c.Name`/`c.IsPrivate`/`c.IsMember` come from `slack.Channel` (which embeds `GroupConversation`→`Conversation`) in slack-go v0.23.1. If any field name differs in the vendored version, build will fail — verify with `go build ./internal/monitor/` and adjust to the actual field names (do not invent fields).

- [ ] **Step 4: Run the monitor test**

Run: `go test ./internal/monitor/ -run TestListSlackChannels -v` → PASS (2). Then `go build ./internal/monitor/`.

- [ ] **Step 5: Write the failing server handler test** — append to `internal/server/attention_test.go`:

```go
func TestHandleSlackChannels(t *testing.T) {
	s, _ := attentionTestServer(t)
	old := listSlackChannelsFn
	listSlackChannelsFn = func(_ context.Context) ([]monitor.SlackChannelInfo, error) {
		return []monitor.SlackChannelInfo{{ID: "C1", Name: "general", IsMember: true}}, nil
	}
	t.Cleanup(func() { listSlackChannelsFn = old })

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
	rec := httptest.NewRecorder()
	s.handleSlackChannels(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var chans []monitor.SlackChannelInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &chans); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(chans) != 1 || chans[0].ID != "C1" {
		t.Errorf("chans = %+v", chans)
	}
}

func TestHandleSlackChannelsError(t *testing.T) {
	s, _ := attentionTestServer(t)
	old := listSlackChannelsFn
	listSlackChannelsFn = func(_ context.Context) ([]monitor.SlackChannelInfo, error) {
		return nil, errors.New("slack down")
	}
	t.Cleanup(func() { listSlackChannelsFn = old })

	req := httptest.NewRequest(http.MethodGet, "/api/slack/channels", nil)
	rec := httptest.NewRecorder()
	s.handleSlackChannels(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
```

Add the imports `"context"`, `"errors"`, and `"flow/internal/monitor"` to `attention_test.go`'s import block.

- [ ] **Step 6: Write the server handler** — append to `internal/server/attention.go`:

Add `"flow/internal/monitor"` to the import block, then:

```go
// listSlackChannelsFn is the mockable seam for the channel-list endpoint.
var listSlackChannelsFn = monitor.ListSlackChannels

// handleSlackChannels serves GET /api/slack/channels — the channel list for
// the steering watch-channel picker.
func (s *Server) handleSlackChannels(w http.ResponseWriter, r *http.Request) {
	if !getOnly(w, r) {
		return
	}
	channels, err := listSlackChannelsFn(r.Context())
	if err != nil {
		writeError(w, err, http.StatusBadGateway)
		return
	}
	if channels == nil {
		channels = []monitor.SlackChannelInfo{}
	}
	writeJSON(w, channels)
}
```

- [ ] **Step 7: Register the route** — in `registerAPIRoutes` (`server.go`), near the other `/api/slack/...` routes:

```go
	mux.HandleFunc("/api/slack/channels", s.handleSlackChannels)
```

- [ ] **Step 8: Run to verify it passes**

Run: `go test ./internal/monitor/ -run TestListSlackChannels && go test ./internal/server/ -run 'TestHandleSlackChannels' -v` → PASS.
Then `go test ./...` (whole module), `go build ./...`, `go build -o flow .`, `go vet ./internal/monitor/ ./internal/server/`, `gofmt -l internal/monitor/slack_channels.go internal/server/attention.go` (no output). Do NOT commit the `flow` binary.

- [ ] **Step 9: Commit**

```bash
git add internal/monitor/slack_channels.go internal/monitor/slack_channels_test.go internal/server/attention.go internal/server/attention_test.go internal/server/server.go
git commit -m "feat: /api/slack/channels endpoint (conversations.list)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage (P1.4a scope):**
- §7 read the feed → `GET /api/attention` + `AttentionItemView`. ✅
- §8 act on a feed item → `attention-act` action (make-task/forward/dismiss, manual bypass via the P1.3 executors). ✅
- §10 watched/muted channel config → `Steering` settings-registry entries (persisted, env-exported). ✅
- §10 settings take effect without restart → `Cascade.ConfigFn` live read + serve wiring. ✅
- §11 channel picker data source → `GET /api/slack/channels` (conversations.list). ✅
- *Deferred to P1.4b (correct):* the React Attention panel, the channel multi-select control, nav badge, and urgent-item notifications. Those consume these endpoints.

**2. Placeholder scan:** No TBD/TODO. The slack-go field-access note (Task 3 Step 3) is a verify-against-real-API instruction, not a placeholder. Every step has complete code.

**3. Type consistency:**
- `AttentionItemView` fields map 1:1 from `flowdb.FeedItem` (P1.1); `attentionItemView` builder is the only producer. ✅
- `actionRequest.AttentionAction` (new) read by `attentionAct`; dispatch `case "attention-act"` returns `(actionResponse, int)` matching every other handler. ✅
- `steering.ApplyAction(ctx, *sql.DB, FeedItem, Action, AutonomyPolicy, bool)` + `steering.DismissFeed(*sql.DB, string)` — match P1.3; `s.cfg.DB` is `*sql.DB`. ✅
- `Cascade.ConfigFn func() WatchConfig`; `Observe` uses it when non-nil; `NewCascade` leaves it nil (backward-compatible — existing cascade tests pass static `Config`). ✅
- `monitor.SlackChannelInfo`/`ListSlackChannels(ctx) ([]SlackChannelInfo, error)`; server's `listSlackChannelsFn = monitor.ListSlackChannels` (mockable); both tests use the same shape. ✅
- Server test harness: `&Server{cfg: Config{DB: db, FlowRoot: root}}` (no `New()`); `runAction`/`publishUIChange` are nil-safe on `s.events`. ✅
- `getOnly`/`writeError`/`writeJSON` reused from routes.go. ✅

No unresolved issues.

---

## After P1.4a

The backend exposes everything the UI needs: list the feed, act on items, list channels, persist watched-channels (live). **P1.4b** (the React surface) is next: a `screens/Attention.tsx` feed panel (cards + action buttons via `apiGet`/`useAction`), a channel multi-select control in Settings (consuming `/api/slack/channels`, saving `FLOW_STEERING_WATCH_CHANNELS` via `update-settings`), a nav entry + badge in `Shell.tsx`, and wiring urgent items into the existing `NotificationsBell`/desktop-notification path. P1.4b should use the **frontend-design skill** and match the dark operator-console aesthetic (tokens.css, `ui.tsx` primitives) — no generic dashboard look.

## Execution Handoff

Plan complete. Execute subagent-driven (recommended) or inline?
