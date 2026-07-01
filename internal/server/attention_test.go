package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
	"flow/internal/steering"
)

func attentionTestServer(t *testing.T) (*Server, *sql.DB) {
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

func seedFeedItem(t *testing.T, db *sql.DB, id, status string) {
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

func TestHandleAttentionIncludesExplainabilityFields(t *testing.T) {
	s, db := attentionTestServer(t)
	now := "2026-06-05T10:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO projects (slug, name, status, priority, work_dir, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"flow-manager", "Flow Manager", "active", "medium", t.TempDir(), now, now,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, priority, work_dir, project_slug, session_provider, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"deploy-followup", "Follow up on deploy thread", "in-progress", "high", t.TempDir(), "flow-manager", "codex", now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	contextJSON := `{
		"source":"slack",
		"thread_key":"C1:1.1",
		"summary":"2 Slack messages from alice, bob",
		"fetch_status":"ok",
		"participants":["alice","bob"],
		"parent":{"kind":"parent","author":"alice","text":"Can we ship the deploy Friday?","ts":"1.1"},
		"messages":[{"kind":"reply","author":"bob","text":"Needs rollback note first.","ts":"1.2"}]
	}`
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "why1", Source: "slack", ThreadKey: "C1:1.1", Summary: "Deploy follow-up",
		SuggestedAction: "forward", MatchedTask: "deploy-followup", SuggestedProject: "flow-manager",
		Confidence: 0.86, Reason: "existing task already owns this deploy thread", ContextJSON: contextJSON,
		Status: "new", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	relevant := true
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "why-trace", CreatedAt: now, Origin: "live", Source: "slack", Channel: "C1", Author: "alice",
		ThreadKey: "C1:1.1", TextPreview: "Can we ship?", Disposition: "surfaced", StageReached: "stage3",
		Stage1Relevant: &relevant, Stage2Action: "reply", Stage2Confidence: 0.74,
		Stage3Action: "forward", Stage3Confidence: 0.92, FinalAction: "forward", FinalConfidence: 0.92,
		FeedItemID: "why1", LatencyMS: 44, Model: "sonnet",
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleAttention(rec, httptest.NewRequest(http.MethodGet, "/api/attention?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	why, ok := rows[0]["why"].(map[string]any)
	if !ok {
		t.Fatalf("why field missing from response: %#v", rows[0])
	}
	if why["context_summary"] != "2 Slack messages from alice, bob" || why["fetch_status"] != "ok" {
		t.Errorf("context evidence = %#v", why)
	}
	if why["stage_reached"] != "stage3" || why["stage_action"] != "forward" || why["stage_confidence"] != 0.92 {
		t.Errorf("stage evidence = %#v", why)
	}
	if why["reason"] != "existing task already owns this deploy thread" || why["confidence"] != 0.86 {
		t.Errorf("reason/confidence evidence = %#v", why)
	}
	if why["parent_preview"] != "Can we ship the deploy Friday?" || why["latest_preview"] != "Needs rollback note first." {
		t.Errorf("message previews = %#v", why)
	}
	match, ok := why["matched_task"].(map[string]any)
	if !ok {
		t.Fatalf("matched_task missing from why field: %#v", why)
	}
	if match["slug"] != "deploy-followup" || match["name"] != "Follow up on deploy thread" ||
		match["status"] != "in-progress" || match["priority"] != "high" || match["project_slug"] != "flow-manager" {
		t.Errorf("matched task evidence = %#v", match)
	}
}

func TestHandleAttentionIncludesActionPreviews(t *testing.T) {
	s, db := attentionTestServer(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "act1", Source: "slack", ThreadKey: "C1:1.1", Summary: "Needs reply",
		SuggestedAction: "forward", MatchedTask: "deploy-followup", Draft: "On it.",
		Channel: "C1", Author: "alice", Confidence: 0.75, Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleAttention(rec, httptest.NewRequest(http.MethodGet, "/api/attention?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	previews, ok := rows[0]["action_previews"].([]any)
	if !ok {
		t.Fatalf("action_previews missing from response: %#v", rows[0])
	}
	byAction := map[string]map[string]any{}
	for _, raw := range previews {
		p, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("bad preview shape: %#v", raw)
		}
		if action, _ := p["action"].(string); action != "" {
			byAction[action] = p
		}
	}
	for _, action := range []string{"make_task", "make_task_start", "confirm_handoff", "forward", "send_reply", "dismiss", "retriage", "mute_channel", "mute_sender", "mute_thread"} {
		if byAction[action] == nil {
			t.Fatalf("missing action preview %q in %#v", action, previews)
		}
	}
	if byAction["confirm_handoff"]["target"] != "deploy-followup" {
		t.Errorf("confirm_handoff target = %#v, want deploy-followup", byAction["confirm_handoff"])
	}
	if byAction["confirm_handoff"]["label"] != "Ask task agent" {
		t.Errorf("confirm_handoff label = %#v, want Ask task agent", byAction["confirm_handoff"])
	}
	if byAction["forward"]["target"] != "deploy-followup" {
		t.Errorf("forward target = %#v, want deploy-followup", byAction["forward"])
	}
	if byAction["forward"]["primary"] != true {
		t.Errorf("forward should be the suggested matched-task action: %#v", byAction["forward"])
	}
	if byAction["confirm_handoff"]["primary"] == true {
		t.Errorf("confirm_handoff should not be the suggested action: %#v", byAction["confirm_handoff"])
	}
	if byAction["send_reply"]["target"] != "source thread" {
		t.Errorf("send_reply target = %#v, want source thread", byAction["send_reply"])
	}
	if desc, _ := byAction["make_task"]["description"].(string); desc == "" {
		t.Errorf("make_task preview should explain the effect: %#v", byAction["make_task"])
	}
}

func TestAttentionItemViewIncludesLatestHandoff(t *testing.T) {
	s, db := attentionTestServer(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "hv1", Source: "slack", ThreadKey: "C1:hv1", Summary: "Needs owner check",
		SuggestedAction: "forward", MatchedTask: "deploy-followup", Confidence: 0.75,
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	h, err := flowdb.CreateAttentionHandoff(db, flowdb.AttentionHandoff{
		FeedItemID: "hv1", Sender: "attention-router", Receiver: "deploy-followup",
		Context: "context", RequestedVerdict: "accept_or_decline",
		RequestedAt: "2099-06-05T10:01:00Z", ExpiresAt: "2099-06-05T10:31:00Z",
	})
	if err != nil {
		t.Fatalf("CreateAttentionHandoff: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleAttention(rec, httptest.NewRequest(http.MethodGet, "/api/attention?status=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hv, ok := rows[0]["handoff"].(map[string]any)
	if !ok {
		t.Fatalf("handoff missing from attention response: %#v", rows[0])
	}
	if hv["id"] != h.ID || hv["status"] != "pending" || hv["receiver"] != "deploy-followup" {
		t.Fatalf("handoff view mismatch: %#v", hv)
	}
}

func TestAttentionActConfirmHandoff(t *testing.T) {
	s, db := attentionTestServer(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "ch1", Source: "slack", ThreadKey: "C1:ch1", Summary: "Needs owner check",
		SuggestedAction: "forward", MatchedTask: "deploy-followup", Confidence: 0.75,
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	old := attentionRequestHandoff
	attentionRequestHandoff = func(_ *Server, item flowdb.FeedItem) (flowdb.AttentionHandoff, error) {
		return flowdb.CreateAttentionHandoff(db, flowdb.AttentionHandoff{
			FeedItemID: item.ID, Sender: "attention-router", Receiver: item.MatchedTask,
			Context: "context", RequestedVerdict: "accept_or_decline",
			RequestedAt: "2026-06-05T10:01:00Z", ExpiresAt: "2026-06-05T10:31:00Z",
		})
	}
	t.Cleanup(func() { attentionRequestHandoff = old })

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "ch1", AttentionAction: "confirm-handoff"})
	if status != 200 || !resp.OK {
		t.Fatalf("confirm-handoff = (%+v, %d), want OK 200", resp, status)
	}
	item, _ := flowdb.GetFeedItem(db, "ch1")
	if item.Status != "new" {
		t.Fatalf("confirm-handoff request must leave card open, got %+v", item)
	}
}

func TestAttentionActMergeIntoRecordsWorkstreamAlias(t *testing.T) {
	s, db := attentionTestServer(t)
	for _, item := range []flowdb.FeedItem{
		{
			ID: "keep", Source: "slack", ThreadKey: "D1:100.0", SuggestedAction: "make_task",
			Summary: "cert-manager IRSA migration", Channel: "D1", ChannelType: "im", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
		},
		{
			ID: "dupe", Source: "slack", ThreadKey: "D1:110.0", SuggestedAction: "make_task",
			Summary: "cert-manager smoke timeout", Channel: "D1", ChannelType: "im", Status: "new", CreatedAt: "2026-06-05T10:01:00Z",
		},
	} {
		if _, err := flowdb.UpsertFeedItem(db, item); err != nil {
			t.Fatalf("seed %s: %v", item.ID, err)
		}
	}

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "dupe", AttentionAction: "merge-into", MergeTarget: "keep"})
	if status != http.StatusOK || !resp.OK {
		t.Fatalf("merge-into = (%+v, %d), want OK 200", resp, status)
	}
	dupe, err := flowdb.GetFeedItem(db, "dupe")
	if err != nil {
		t.Fatalf("GetFeedItem: %v", err)
	}
	if dupe.Status != "acted" {
		t.Fatalf("dupe status = %q, want acted", dupe.Status)
	}
	ws, ok, err := flowdb.AttentionWorkstreamByThreadKey(db, "D1:110.0")
	if err != nil || !ok {
		t.Fatalf("alias ok=%v err=%v", ok, err)
	}
	if ws.CanonicalFeedItemID != "keep" || ws.CanonicalThreadKey != "D1:100.0" {
		t.Fatalf("workstream = %+v, want keep/D1:100.0", ws)
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
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "d1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "dismiss" || fb[0].Outcome != "dismissed" {
		t.Errorf("dismiss feedback mismatch: %+v", fb)
	}
}

func TestAttentionActMakeTaskStart(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "ms1", "new")

	// Stub the spawn + session-open seams so the test stays hermetic (no real
	// `flow spawn`, no PTY). The make-task seam marks the row acted+linked.
	oldMake, oldStart := attentionMakeTask, attentionStartSession
	startedCh := make(chan string, 4) // buffered: session start is now async (goroutine)
	attentionMakeTask = func(srv *Server, item flowdb.FeedItem) error {
		return flowdb.SetFeedItemActed(srv.cfg.DB, item.ID, steering.FeedTaskSlug(item), "2026-06-05T11:00:00Z")
	}
	attentionStartSession = func(_ *Server, slug string) error { startedCh <- slug; return nil }
	t.Cleanup(func() { attentionMakeTask, attentionStartSession = oldMake, oldStart })

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "ms1", AttentionAction: "make-task-start"})
	if status != 200 || !resp.OK {
		t.Fatalf("runAction = (%+v, %d), want OK 200", resp, status)
	}
	item, _ := flowdb.GetFeedItem(db, "ms1")
	wantSlug := steering.FeedTaskSlug(item)
	// The action returns immediately; the open runs in the background.
	select {
	case startedSlug := <-startedCh:
		if startedSlug != wantSlug {
			t.Errorf("started session for %q, want %q", startedSlug, wantSlug)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background session start did not fire within 2s")
	}
	if item.Status != "acted" || item.LinkedTask != wantSlug {
		t.Errorf("feed row = status %q linked %q, want acted/%s", item.Status, item.LinkedTask, wantSlug)
	}
	// Underscore alias is also recognized.
	seedFeedItem(t, db, "ms2", "new")
	if _, status := s.runAction(actionRequest{Kind: "attention-act", Target: "ms2", AttentionAction: "make_task_start"}); status != 200 {
		t.Errorf("make_task_start alias → %d, want 200", status)
	}
}

func TestAttentionActMakeTaskStartOpenBestEffort(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "be1", "new")

	oldMake, oldStart := attentionMakeTask, attentionStartSession
	attentionMakeTask = func(srv *Server, item flowdb.FeedItem) error {
		return flowdb.SetFeedItemActed(srv.cfg.DB, item.ID, steering.FeedTaskSlug(item), "2026-06-05T11:00:00Z")
	}
	attentionStartSession = func(_ *Server, _ string) error { return errors.New("pty down") }
	t.Cleanup(func() { attentionMakeTask, attentionStartSession = oldMake, oldStart })

	// Task creation succeeded; the open is best-effort, so the action still
	// reports OK (200) even though the session couldn't be auto-opened.
	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "be1", AttentionAction: "make-task-start"})
	if status != 200 || !resp.OK {
		t.Fatalf("best-effort open: runAction = (%+v, %d), want OK 200", resp, status)
	}
	if item, _ := flowdb.GetFeedItem(db, "be1"); item.Status != "acted" {
		t.Errorf("feed row should still be acted, got %q", item.Status)
	}
}

func seedReplyFeedItem(t *testing.T, db *sql.DB, id, matchedTask, draft string) {
	t.Helper()
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: id, Source: "slack", ThreadKey: "C1:" + id, Summary: "s-" + id,
		SuggestedAction: "send_reply", MatchedTask: matchedTask, Draft: draft,
		Confidence: 0.8, Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed reply %s: %v", id, err)
	}
}

// NOTE on hermeticity: send-reply routes through steering.InjectReplyToTask /
// MakeReplyTaskFromFeed, which shell out to the real `flow tell` / `flow spawn`
// via steering's UNEXPORTED taskTeller/taskSpawner seams — those aren't
// reachable from this (server) package, so we can't stub them here. The full
// behavioural assertions (the agent-mediated inject/spawn, feed row acted+
// linked, reply text embedded) live in internal/steering/actions_test.go
// (TestInjectReplyToTask, TestMakeReplyTaskFromFeed) where the seams ARE
// accessible. These server tests therefore stay on the HTTP contract: the verb
// is recognized and the draft/reply_text gate behaves correctly — without
// depending on the real `flow` shell-out succeeding.

func TestAttentionSendReplyEmpty(t *testing.T) {
	s, db := attentionTestServer(t)
	// No draft, no reply_text → 400 before any side effect (no shell-out).
	seedReplyFeedItem(t, db, "sre1", "", "")

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "sre1", AttentionAction: "send-reply"})
	if status != http.StatusBadRequest || resp.OK {
		t.Fatalf("empty draft → (%+v, %d), want not-OK 400", resp, status)
	}
	if item, _ := flowdb.GetFeedItem(db, "sre1"); item.Status != "new" {
		t.Errorf("feed row should be untouched ('new'), got %q", item.Status)
	}
	// Underscore alias is also recognized as send-reply (still 400 on empty).
	if _, status := s.runAction(actionRequest{Kind: "attention-act", Target: "sre1", AttentionAction: "send_reply"}); status != http.StatusBadRequest {
		t.Errorf("send_reply alias empty → %d, want 400", status)
	}
}

func TestAttentionSendReplyRecognized(t *testing.T) {
	s, db := attentionTestServer(t)
	// A draft is present, so the verb is recognized and routed PAST the
	// empty-draft 400 gate (it does NOT fall through to the unknown-verb 400).
	// This is a SLACK card: a Slack reply now always routes through the channel's
	// per-channel chat / an ephemeral Slack session (never the matched task), and
	// both need the terminal hub — which this hermetic server lacks, so the
	// definitive answer is 503. We assert the contract: the verb is recognized
	// (status != 400) and we get a definitive answer (200 sent / 500 shell-out /
	// 503 no hub).
	seedReplyFeedItem(t, db, "srr1", "t1", "stored draft")

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "srr1", AttentionAction: "send-reply"})
	if status == http.StatusBadRequest {
		t.Fatalf("recognized verb with a draft must not 400; got (%+v, %d)", resp, status)
	}
	if status != http.StatusOK && status != http.StatusInternalServerError && status != http.StatusServiceUnavailable {
		t.Fatalf("send-reply → %d, want 200 (agent sent) / 500 (shell-out) / 503 (no hub in test)", status)
	}
}

func TestAttentionSendReplyEditedTextOverridesEmptyDraft(t *testing.T) {
	s, db := attentionTestServer(t)
	// No stored draft, but the operator supplies edited reply_text → the
	// empty-draft 400 must NOT fire (reply_text is preferred over Draft). The
	// downstream shell-out may then fail in CI; either way it's not a 400.
	seedReplyFeedItem(t, db, "sed1", "t9", "")

	resp, status := s.runAction(actionRequest{
		Kind: "attention-act", Target: "sed1", AttentionAction: "send_reply",
		ReplyText: "operator edited this",
	})
	if status == http.StatusBadRequest {
		t.Fatalf("reply_text must override an empty draft (no 400); got (%+v, %d)", resp, status)
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

func TestAttentionActMuteRecordsFeedback(t *testing.T) {
	s, db := attentionTestServer(t)
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "mute1", Source: "slack", ThreadKey: "C_MUTED:1.1", Summary: "noise",
		SuggestedAction: "reply", Confidence: 0.8, Status: "new", Channel: "C_MUTED",
		ChannelType: "channel", Author: "U_NOISE", CreatedAt: "2026-06-05T10:00:00Z",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "mute1", AttentionAction: "mute-channel"})
	if status != 200 || !resp.OK {
		t.Fatalf("runAction = (%+v, %d), want OK 200", resp, status)
	}
	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "mute1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 1 || fb[0].FinalAction != "mute_channel" || fb[0].Outcome != "muted" {
		t.Errorf("mute feedback mismatch: %+v", fb)
	}
}

func TestAttentionActRetriageAndOpenSignalsRecordFeedback(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "sig1", "new")
	s.cascade = steering.NewCascade(db, steering.WatchConfig{})
	oldLaunch := launchAttentionRetriage
	launched := false
	launchAttentionRetriage = func(s *Server, item flowdb.FeedItem) {
		launched = true
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, item.ID, "")
	}
	t.Cleanup(func() { launchAttentionRetriage = oldLaunch })

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "sig1", AttentionAction: "retriage"})
	if status != 200 || !resp.OK {
		t.Fatalf("retriage = (%+v, %d), want OK 200", resp, status)
	}
	if !launched {
		t.Fatal("retriage launcher was not called")
	}
	resp, status = s.runAction(actionRequest{Kind: "attention-act", Target: "sig1", AttentionAction: "open-source"})
	if status != 200 || !resp.OK {
		t.Fatalf("open-source = (%+v, %d), want OK 200", resp, status)
	}

	fb, err := flowdb.ListAttentionFeedback(db, flowdb.AttentionFeedbackFilter{FeedItemID: "sig1"})
	if err != nil {
		t.Fatalf("ListAttentionFeedback: %v", err)
	}
	if len(fb) != 2 {
		t.Fatalf("feedback rows = %d, want 2: %+v", len(fb), fb)
	}
	seen := map[string]string{}
	for _, row := range fb {
		seen[row.FinalAction] = row.Outcome
	}
	if seen["retriage"] != "retriaged" || seen["open_source"] != "opened" {
		t.Errorf("feedback signals mismatch: %+v", fb)
	}
}

func TestAttentionActCorrectionRetriageUsesCorrectionPath(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "corr-ret", "new")
	s.cascade = steering.NewCascade(db, steering.WatchConfig{})
	oldLaunch := launchAttentionCorrectionRetriage
	launched := false
	launchAttentionCorrectionRetriage = func(s *Server, item flowdb.FeedItem) {
		launched = true
		_ = flowdb.SetFeedRetriaging(s.cfg.DB, item.ID, "")
	}
	t.Cleanup(func() { launchAttentionCorrectionRetriage = oldLaunch })

	resp, status := s.runAction(actionRequest{Kind: "attention-act", Target: "corr-ret", AttentionAction: "correction-retriage"})
	if status != 200 || !resp.OK {
		t.Fatalf("correction-retriage = (%+v, %d), want OK 200", resp, status)
	}
	if !launched {
		t.Fatal("correction retriage launcher was not called")
	}
}

func TestAttentionItemView(t *testing.T) {
	s, _ := attentionTestServer(t) // nil nameResolver
	v := s.attentionItemView(context.Background(), flowdb.FeedItem{ID: "x", Source: "slack", ThreadKey: "C1:1.1", Summary: "hi <@U1>", SuggestedAction: "reply", Confidence: 0.5, Status: "new", CreatedAt: "2026-06-05T10:00:00Z"})
	if v.ID != "x" || v.Source != "slack" || v.SuggestedAction != "reply" || v.Confidence != 0.5 {
		t.Errorf("view = %+v", v)
	}
	// With a nil resolver the text passes through unchanged (nil-safe).
	if v.Summary != "hi <@U1>" {
		t.Errorf("Summary with nil resolver = %q, want unchanged", v.Summary)
	}
}

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

func seedTrace(t *testing.T, db *sql.DB, id, disposition, stage, createdAt string) {
	t.Helper()
	tr := flowdb.SteeringTrace{
		ID: id, CreatedAt: createdAt, Origin: "slack", Source: "slack",
		Channel: "C1", ChannelType: "channel", Author: "u1", ThreadKey: "C1:" + id,
		TextPreview: "preview-" + id, Disposition: disposition, StageReached: stage,
		LatencyMS: 42,
	}
	if id == "tr1" {
		tr.AutonomyAction = "make_task"
		tr.AutonomyDecision = "acted"
		tr.AutonomyReason = "confidence 0.95 >= threshold 0.80"
	}
	if err := flowdb.InsertSteeringTrace(db, tr); err != nil {
		t.Fatalf("seedTrace %s: %v", id, err)
	}
}

func TestHandleAttentionTrace(t *testing.T) {
	s, db := attentionTestServer(t)

	// Seed 4 rows inside the since window and 1 older row.
	seedTrace(t, db, "tr1", "surfaced", "stage3", "2026-06-05T10:00:00Z")
	seedTrace(t, db, "tr2", "dropped", "stage0", "2026-06-05T10:01:00Z")
	seedTrace(t, db, "tr3", "dropped", "stage1", "2026-06-05T10:02:00Z")
	seedTrace(t, db, "tr4", "error", "stage2", "2026-06-05T10:03:00Z")
	seedTrace(t, db, "tr5", "dropped", "cache", "2026-06-01T10:00:00Z") // older — excluded

	since := "2026-06-05T00:00:00Z"

	// --- baseline: all rows in window ----------------------------------------
	req := httptest.NewRequest(http.MethodGet, "/api/attention/trace?since="+since, nil)
	rec := httptest.NewRecorder()
	s.handleAttentionTrace(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp AttentionTraceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Funnel should count 4 rows (tr1–tr4), not tr5.
	if resp.Funnel.Observed != 4 {
		t.Errorf("Funnel.Observed = %d, want 4", resp.Funnel.Observed)
	}
	if resp.Funnel.Surfaced != 1 {
		t.Errorf("Funnel.Surfaced = %d, want 1", resp.Funnel.Surfaced)
	}
	if resp.Funnel.DroppedStage0 != 1 {
		t.Errorf("Funnel.DroppedStage0 = %d, want 1", resp.Funnel.DroppedStage0)
	}
	if resp.Funnel.DroppedStage1 != 1 {
		t.Errorf("Funnel.DroppedStage1 = %d, want 1", resp.Funnel.DroppedStage1)
	}
	if resp.Funnel.Errors != 1 {
		t.Errorf("Funnel.Errors = %d, want 1", resp.Funnel.Errors)
	}
	if len(resp.Items) != 4 {
		t.Errorf("len(Items) = %d, want 4", len(resp.Items))
	}
	var surfaced *SteeringTraceView
	for i := range resp.Items {
		if resp.Items[i].ID == "tr1" {
			surfaced = &resp.Items[i]
			break
		}
	}
	if surfaced == nil {
		t.Fatalf("tr1 missing from trace response: %+v", resp.Items)
	}
	if surfaced.AutonomyAction != "make_task" || surfaced.AutonomyDecision != "acted" || surfaced.AutonomyReason == "" {
		t.Errorf("autonomy audit fields = action %q decision %q reason %q", surfaced.AutonomyAction, surfaced.AutonomyDecision, surfaced.AutonomyReason)
	}

	// --- disposition filter ---------------------------------------------------
	req2 := httptest.NewRequest(http.MethodGet, "/api/attention/trace?since="+since+"&disposition=dropped", nil)
	rec2 := httptest.NewRecorder()
	s.handleAttentionTrace(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("disposition filter status = %d, want 200", rec2.Code)
	}
	var resp2 AttentionTraceResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode resp2: %v", err)
	}
	for _, item := range resp2.Items {
		if item.Disposition != "dropped" {
			t.Errorf("disposition filter: got item with disposition=%q, want dropped", item.Disposition)
		}
	}
	if len(resp2.Items) != 2 {
		t.Errorf("disposition=dropped: len(Items) = %d, want 2", len(resp2.Items))
	}

	// --- limit filter ---------------------------------------------------------
	req3 := httptest.NewRequest(http.MethodGet, "/api/attention/trace?since="+since+"&limit=1", nil)
	rec3 := httptest.NewRecorder()
	s.handleAttentionTrace(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("limit filter status = %d, want 200", rec3.Code)
	}
	var resp3 AttentionTraceResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("decode resp3: %v", err)
	}
	if len(resp3.Items) != 1 {
		t.Errorf("limit=1: len(Items) = %d, want 1", len(resp3.Items))
	}

	// --- POST should be rejected ----------------------------------------------
	req4 := httptest.NewRequest(http.MethodPost, "/api/attention/trace", nil)
	rec4 := httptest.NewRecorder()
	s.handleAttentionTrace(rec4, req4)
	if rec4.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST → %d, want 405", rec4.Code)
	}
}

func TestHandleAttentionDecision(t *testing.T) {
	s, db := attentionTestServer(t)
	seedFeedItem(t, db, "fd1", "new")
	// A trace whose feed_item_id points at the seeded feed item.
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "dt1", CreatedAt: "2026-06-05T10:00:00Z", Origin: "live", Source: "slack",
		Channel: "C1", ChannelType: "channel", Author: "u1", ThreadKey: "C1:fd1",
		TextPreview: "why", Disposition: "surfaced", StageReached: "stage3",
		FinalAction: "forward", FeedItemID: "fd1", LatencyMS: 12,
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	// --- happy path ----------------------------------------------------------
	req := httptest.NewRequest(http.MethodGet, "/api/attention/decision?feed_id=fd1", nil)
	rec := httptest.NewRecorder()
	s.handleAttentionDecision(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var view SteeringTraceView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.ID != "dt1" || view.FinalAction != "forward" || view.FeedItemID != "fd1" {
		t.Errorf("view = %+v, want dt1/forward/fd1", view)
	}

	// --- missing feed_id → 400 -----------------------------------------------
	rec2 := httptest.NewRecorder()
	s.handleAttentionDecision(rec2, httptest.NewRequest(http.MethodGet, "/api/attention/decision", nil))
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("missing feed_id → %d, want 400", rec2.Code)
	}

	// --- unknown feed_id → 404 -----------------------------------------------
	rec3 := httptest.NewRecorder()
	s.handleAttentionDecision(rec3, httptest.NewRequest(http.MethodGet, "/api/attention/decision?feed_id=ghost", nil))
	if rec3.Code != http.StatusNotFound {
		t.Errorf("unknown feed_id → %d, want 404", rec3.Code)
	}

	// --- POST rejected -------------------------------------------------------
	rec4 := httptest.NewRecorder()
	s.handleAttentionDecision(rec4, httptest.NewRequest(http.MethodPost, "/api/attention/decision?feed_id=fd1", nil))
	if rec4.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST → %d, want 405", rec4.Code)
	}
}

func TestAttentionTraceIncludesForwardedTaskTarget(t *testing.T) {
	s, db := attentionTestServer(t)
	now := "2026-06-05T10:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO tasks (slug,name,status,priority,work_dir,session_provider,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		"raptor-review", "Raptor PR review", "in-progress", "high", t.TempDir(), "codex", now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, err := flowdb.UpsertFeedItem(db, flowdb.FeedItem{
		ID: "ft1", Source: "github", ThreadKey: "facets-cloud/raptor:gh-pr:facets-cloud/raptor#159",
		Summary: "Raptor PR moved", SuggestedAction: "forward", MatchedTask: "raptor-review",
		Confidence: 0.93, Status: "acted", LinkedTask: "raptor-review", CreatedAt: now, ActedAt: now,
	}); err != nil {
		t.Fatalf("seed feed: %v", err)
	}
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "ft-trace", CreatedAt: now, Origin: "live", Source: "github", Channel: "facets-cloud/raptor",
		ThreadKey: "facets-cloud/raptor:gh-pr:facets-cloud/raptor#159", TextPreview: "head changed",
		Disposition: "surfaced", StageReached: "stage3", FinalAction: "forward", FinalConfidence: 0.93,
		FeedItemID: "ft1", AutonomyAction: "forward", AutonomyDecision: "acted",
		AutonomyReason: "forward allowed", LatencyMS: 10,
	}); err != nil {
		t.Fatalf("seed trace: %v", err)
	}

	rec := httptest.NewRecorder()
	s.handleAttentionTrace(rec, httptest.NewRequest(http.MethodGet, "/api/attention/trace?since=2026-06-05T00:00:00Z", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("trace status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var traceResp AttentionTraceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &traceResp); err != nil {
		t.Fatalf("decode trace response: %v", err)
	}
	if len(traceResp.Items) != 1 {
		t.Fatalf("trace items = %d, want 1: %+v", len(traceResp.Items), traceResp.Items)
	}
	trace := traceResp.Items[0]
	if trace.LinkedTask != "raptor-review" {
		t.Fatalf("trace linked task = %q, want raptor-review", trace.LinkedTask)
	}
	if trace.MatchedTask == nil || trace.MatchedTask.Slug != "raptor-review" || trace.MatchedTask.Name != "Raptor PR review" {
		t.Fatalf("trace matched task = %+v, want raptor-review details", trace.MatchedTask)
	}

	rec2 := httptest.NewRecorder()
	s.handleAttentionDecision(rec2, httptest.NewRequest(http.MethodGet, "/api/attention/decision?feed_id=ft1", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("decision status = %d, want 200; body: %s", rec2.Code, rec2.Body.String())
	}
	var decision SteeringTraceView
	if err := json.Unmarshal(rec2.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision response: %v", err)
	}
	if decision.LinkedTask != "raptor-review" {
		t.Fatalf("decision linked task = %q, want raptor-review", decision.LinkedTask)
	}
	if decision.MatchedTask == nil || decision.MatchedTask.Slug != "raptor-review" || decision.MatchedTask.Name != "Raptor PR review" {
		t.Fatalf("decision matched task = %+v, want raptor-review details", decision.MatchedTask)
	}
}

func TestHandleAttentionTraceSourceFilter(t *testing.T) {
	s, db := attentionTestServer(t)
	// Two slack traces (seedTrace defaults to slack) + one github.
	seedTrace(t, db, "sa1", "surfaced", "stage3", "2026-06-05T10:00:00Z")
	seedTrace(t, db, "sa2", "dropped", "stage1", "2026-06-05T10:01:00Z")
	if err := flowdb.InsertSteeringTrace(db, flowdb.SteeringTrace{
		ID: "ga1", CreatedAt: "2026-06-05T10:02:00Z", Origin: "live", Source: "github",
		Channel: "o/r", Author: "octocat", ThreadKey: "o/r:gh-pr:o/r#5",
		TextPreview: "review", Disposition: "surfaced", StageReached: "stage3",
		URL: "https://github.com/o/r/pull/5", LatencyMS: 7,
	}); err != nil {
		t.Fatalf("seed github trace: %v", err)
	}

	since := "2026-06-05T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, "/api/attention/trace?since="+since+"&source=github", nil)
	rec := httptest.NewRecorder()
	s.handleAttentionTrace(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp AttentionTraceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].ID != "ga1" {
		t.Errorf("source=github → %d items, want only ga1", len(resp.Items))
	}
	for _, it := range resp.Items {
		if it.Source != "github" {
			t.Errorf("source=github returned a %q row (id=%s)", it.Source, it.ID)
		}
	}
	// The funnel is intentionally NOT source-filtered (overview over the window):
	// it counts all 3 rows.
	if resp.Funnel.Observed != 3 {
		t.Errorf("Funnel.Observed = %d, want 3 (funnel spans the whole window, not the source filter)", resp.Funnel.Observed)
	}

	// source=all behaves like no filter.
	rec2 := httptest.NewRecorder()
	s.handleAttentionTrace(rec2, httptest.NewRequest(http.MethodGet, "/api/attention/trace?since="+since+"&source=all", nil))
	if rec2.Code != 200 {
		t.Fatalf("source=all status = %d, want 200", rec2.Code)
	}
	var resp2 AttentionTraceResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode resp2: %v", err)
	}
	if len(resp2.Items) != 3 {
		t.Errorf("source=all → %d items, want all 3", len(resp2.Items))
	}
}

func TestConnectorPermalink(t *testing.T) {
	// Slack: full team/channel/ts → deep link.
	if got, want := connectorPermalink("slack", "T1", "C1", "123.45", ""), "slack://channel?team=T1&id=C1&message=123.45"; got != want {
		t.Errorf("slack permalink = %q, want %q", got, want)
	}
	// GitHub: the item URL is the canonical permalink (team/channel/ts ignored).
	if got, want := connectorPermalink("github", "", "o/r", "", "https://github.com/o/r/pull/5"), "https://github.com/o/r/pull/5"; got != want {
		t.Errorf("github permalink = %q, want %q", got, want)
	}
	// Slack missing bits → empty.
	if got := connectorPermalink("slack", "", "C1", "123.45", ""); got != "" {
		t.Errorf("slack missing team: permalink = %q, want empty", got)
	}
	if got := connectorPermalink("slack", "T1", "", "123.45", ""); got != "" {
		t.Errorf("slack missing channel: permalink = %q, want empty", got)
	}
	// GitHub with no URL → empty.
	if got := connectorPermalink("github", "T1", "C1", "123.45", ""); got != "" {
		t.Errorf("github no url: permalink = %q, want empty", got)
	}
}

func TestAttentionItemViewSourceContext(t *testing.T) {
	s, _ := attentionTestServer(t) // nil nameResolver

	// Slack channel message: nil resolver leaves names empty, but Channel/
	// ChannelType/Author pass through and a slack permalink is built.
	v := s.attentionItemView(context.Background(), flowdb.FeedItem{
		ID: "x1", Source: "slack", ThreadKey: "C700:700.1", SuggestedAction: "reply",
		Channel: "C700", ChannelType: "channel", Author: "U_BOB", TS: "700.1", TeamID: "T1",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	})
	if v.Channel != "C700" || v.ChannelType != "channel" || v.Author != "U_BOB" {
		t.Errorf("source-context passthrough mismatch: %+v", v)
	}
	if v.Permalink != "slack://channel?team=T1&id=C700&message=700.1" {
		t.Errorf("Permalink = %q, want slack deep link", v.Permalink)
	}
	if v.ChannelName != "" || v.AuthorName != "" {
		t.Errorf("nil resolver should leave names empty for a channel msg, got name=%q author=%q", v.ChannelName, v.AuthorName)
	}

	// Slack DM (im) with no resolver → ChannelName falls back to "Direct message".
	dm := s.attentionItemView(context.Background(), flowdb.FeedItem{
		ID: "x2", Source: "slack", ThreadKey: "D30:30.1", SuggestedAction: "reply",
		Channel: "D30", ChannelType: "im", Author: "U_BOB", TS: "30.1", TeamID: "T1",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	})
	if dm.ChannelName != "Direct message" {
		t.Errorf("im with nil resolver: ChannelName = %q, want %q", dm.ChannelName, "Direct message")
	}

	// GitHub: channel/author are already human; URL is the permalink.
	gh := s.attentionItemView(context.Background(), flowdb.FeedItem{
		ID: "x3", Source: "github", ThreadKey: "o/r:gh-pr:o/r#5", SuggestedAction: "reply",
		Channel: "o/r", Author: "octocat", URL: "https://github.com/o/r/pull/5",
		Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	})
	if gh.ChannelName != "o/r" || gh.AuthorName != "octocat" {
		t.Errorf("github human passthrough mismatch: ChannelName=%q AuthorName=%q", gh.ChannelName, gh.AuthorName)
	}
	if gh.Permalink != "https://github.com/o/r/pull/5" {
		t.Errorf("github Permalink = %q, want the item url", gh.Permalink)
	}
}

func TestAttentionItemViewScrubsStoredInternalFetchDetails(t *testing.T) {
	s, _ := attentionTestServer(t)
	item := flowdb.FeedItem{
		ID: "leaky", Source: "slack", ThreadKey: "C1:1.2", SuggestedAction: "digest_only",
		Summary: "Slack context fetch failed (not_in_channel), but leave notice is FYI-only.",
		Reason:  "Plain leave notice. Slack context fetch failed (not_in_channel) so the sender's name couldn't be resolved, but the message text is self-contained and clearly FYI-only. Worth surfacing in a digest.",
		Status:  "new", CreatedAt: "2026-06-05T10:00:00Z",
	}
	v := s.attentionItemView(context.Background(), item)
	for _, field := range []struct {
		name string
		text string
	}{
		{"summary", v.Summary},
		{"reason", v.Reason},
		{"why.reason", v.Why.Reason},
	} {
		for _, leak := range []string{"Slack context fetch failed", "not_in_channel", "sender's name couldn't be resolved"} {
			if strings.Contains(field.text, leak) {
				t.Fatalf("%s leaked %q: %q", field.name, leak, field.text)
			}
		}
	}
	if !strings.Contains(v.Reason, "message text is self-contained") || !strings.Contains(v.Why.Reason, "Worth surfacing in a digest") {
		t.Fatalf("scrubbed reason lost useful rationale: reason=%q why=%q", v.Reason, v.Why.Reason)
	}
}

func TestSplitThreadKey(t *testing.T) {
	cases := []struct{ in, wantCh, wantTS string }{
		{"D075KEWD1H9:1780653157.014339", "D075KEWD1H9", "1780653157.014339"},
		{"C1:123.45", "C1", "123.45"},
		{"", "", ""},
		{"no-colon", "", ""},
		{":123.45", "", ""}, // leading colon → no channel
	}
	for _, c := range cases {
		ch, ts := splitThreadKey(c.in)
		if ch != c.wantCh || ts != c.wantTS {
			t.Errorf("splitThreadKey(%q) = (%q, %q), want (%q, %q)", c.in, ch, ts, c.wantCh, c.wantTS)
		}
	}
}

func TestAttentionItemViewDerivesFromThreadKey(t *testing.T) {
	s, _ := attentionTestServer(t) // nil nameResolver AND nil slackPermalinker
	if s.slackPermalinker != nil {
		t.Fatal("expected nil slackPermalinker on the test server")
	}

	// Channel/TS are empty (pre-capture item) but thread_key carries them. The
	// derivation should populate v.Channel from thread_key and, with the
	// permalinker nil, fall back to the slack:// deep link built from TeamID +
	// the derived channel/ts — proving thread_key feeds the permalink.
	v := s.attentionItemView(context.Background(), flowdb.FeedItem{
		ID: "tk1", Source: "slack", ThreadKey: "C1:1.2", SuggestedAction: "reply",
		TeamID: "T1", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
		// Channel, TS deliberately empty.
	})
	if v.Channel != "C1" {
		t.Errorf("v.Channel = %q, want %q (derived from thread_key)", v.Channel, "C1")
	}
	if v.Permalink != "slack://channel?team=T1&id=C1&message=1.2" {
		t.Errorf("v.Permalink = %q, want slack deep link from derived channel/ts", v.Permalink)
	}

	// Stored Channel/TS take precedence over thread_key when present.
	v2 := s.attentionItemView(context.Background(), flowdb.FeedItem{
		ID: "tk2", Source: "slack", ThreadKey: "C1:1.2", SuggestedAction: "reply",
		Channel: "C999", TS: "9.9", TeamID: "T1", Status: "new", CreatedAt: "2026-06-05T10:00:00Z",
	})
	if v2.Channel != "C999" {
		t.Errorf("v2.Channel = %q, want stored %q", v2.Channel, "C999")
	}
	if v2.Permalink != "slack://channel?team=T1&id=C999&message=9.9" {
		t.Errorf("v2.Permalink = %q, want deep link from stored channel/ts", v2.Permalink)
	}
}

func TestSteeringTraceViewDerivesFromThreadKey(t *testing.T) {
	s, _ := attentionTestServer(t) // nil nameResolver AND nil slackPermalinker

	// Pre-capture trace: Channel/TS empty, thread_key carries them. The trace
	// view should derive channel/ts and, with the permalinker nil, fall back to
	// the slack:// deep link built from TeamID + derived channel/ts.
	v := s.steeringTraceView(context.Background(), flowdb.SteeringTrace{
		ID: "trk1", Source: "slack", ThreadKey: "C1:1.2", TeamID: "T1",
		TextPreview: "hi", CreatedAt: "2026-06-05T10:00:00Z",
	})
	if v.Channel != "C1" {
		t.Errorf("v.Channel = %q, want %q (derived from thread_key)", v.Channel, "C1")
	}
	if v.Permalink != "slack://channel?team=T1&id=C1&message=1.2" {
		t.Errorf("v.Permalink = %q, want slack deep link from derived channel/ts", v.Permalink)
	}
}

func TestSteeringPermalink(t *testing.T) {
	got := steeringPermalink(flowdb.SteeringTrace{Source: "slack", TeamID: "T1", Channel: "C1", TS: "123.45"})
	want := "slack://channel?team=T1&id=C1&message=123.45"
	if got != want {
		t.Errorf("permalink = %q, want %q", got, want)
	}
	// Missing team → empty.
	if got := steeringPermalink(flowdb.SteeringTrace{Source: "slack", Channel: "C1", TS: "123.45"}); got != "" {
		t.Errorf("missing team: permalink = %q, want empty", got)
	}
	// Non-slack source → empty.
	if got := steeringPermalink(flowdb.SteeringTrace{Source: "github", TeamID: "T1", Channel: "C1", TS: "123.45"}); got != "" {
		t.Errorf("non-slack source: permalink = %q, want empty", got)
	}
}

func TestSteeringTraceViewGitHub(t *testing.T) {
	s, _ := attentionTestServer(t) // nil nameResolver
	tr := flowdb.SteeringTrace{
		Source: "github", Channel: "o/r", Author: "octocat",
		TextPreview: "please review", URL: "https://github.com/o/r/pull/5",
	}
	v := s.steeringTraceView(context.Background(), tr)
	if v.ChannelName != "o/r" {
		t.Errorf("ChannelName = %q, want %q (github repo is already human)", v.ChannelName, "o/r")
	}
	if v.AuthorName != "octocat" {
		t.Errorf("AuthorName = %q, want %q (github login is already human)", v.AuthorName, "octocat")
	}
	if v.Text != "please review" {
		t.Errorf("Text = %q, want %q", v.Text, "please review")
	}
	if v.Permalink != "https://github.com/o/r/pull/5" {
		t.Errorf("Permalink = %q, want the GitHub url", v.Permalink)
	}
}

func TestAttentionTraceResolvesNames(t *testing.T) {
	s, db := attentionTestServer(t)
	// The test server leaves nameResolver nil — exercise the graceful path.
	if s.nameResolver != nil {
		t.Fatal("expected nil nameResolver on the test server")
	}
	seedTrace(t, db, "rn1", "surfaced", "stage3", "2026-06-05T10:00:00Z")

	since := "2026-06-05T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, "/api/attention/trace?since="+since, nil)
	rec := httptest.NewRecorder()
	s.handleAttentionTrace(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp AttentionTraceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len(Items) = %d, want 1", len(resp.Items))
	}
	it := resp.Items[0]
	// With a nil resolver, names stay empty and Text falls back to the preview.
	if it.ChannelName != "" {
		t.Errorf("ChannelName = %q, want empty (nil resolver)", it.ChannelName)
	}
	if it.AuthorName != "" {
		t.Errorf("AuthorName = %q, want empty (nil resolver)", it.AuthorName)
	}
	if it.Text != "preview-rn1" {
		t.Errorf("Text = %q, want fallback to preview %q", it.Text, "preview-rn1")
	}
}
