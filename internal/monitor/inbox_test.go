package monitor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inboxTestSlug picks a slug-shaped string and points TaskDir at a fresh
// temp dir by setting FLOW_ROOT. Tests get isolated filesystem state and
// never touch the user's real ~/.flow.
func inboxTestSlug(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	slug := "test-task-" + strings.ToLower(t.Name())
	return slug
}

func TestInboxPaths_FollowFlowRoot(t *testing.T) {
	slug := inboxTestSlug(t)
	root := os.Getenv("FLOW_ROOT")
	wantDir := filepath.Join(root, "tasks", slug)
	if got := TaskDir(slug); got != wantDir {
		t.Errorf("TaskDir = %q, want %q", got, wantDir)
	}
	if got := InboxPath(slug); got != filepath.Join(wantDir, "inbox.jsonl") {
		t.Errorf("InboxPath = %q", got)
	}
	if got := CursorPath(slug); got != filepath.Join(wantDir, "inbox.cursor") {
		t.Errorf("CursorPath = %q", got)
	}
}

func TestAppendInboxEvent_CreatesFileAndDir(t *testing.T) {
	slug := inboxTestSlug(t)
	ev := InboundEvent{
		Kind:     "message",
		Channel:  "C123",
		TS:       "1234.0001",
		ThreadTS: "1234.0001",
		UserID:   "U999",
		Text:     "hello",
	}
	if err := AppendInboxEvent(slug, ev); err != nil {
		t.Fatalf("Append err = %v", err)
	}
	// File must exist
	if _, err := os.Stat(InboxPath(slug)); err != nil {
		t.Fatalf("inbox.jsonl not created: %v", err)
	}
}

func TestAppendInboxEvent_AppendsMultipleLines(t *testing.T) {
	slug := inboxTestSlug(t)
	for i, text := range []string{"first", "second", "third"} {
		ev := InboundEvent{
			Kind:    "message",
			Channel: "C123",
			TS:      "1234.000" + string(rune('0'+i)),
			Text:    text,
		}
		if err := AppendInboxEvent(slug, ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[0].Event.Text != "first" || entries[2].Event.Text != "third" {
		t.Errorf("entries out of order: %+v", entries)
	}
	if entries[0].EnqueuedAt == "" {
		t.Errorf("EnqueuedAt empty: %+v", entries[0])
	}
}

func TestAppendInboxEvent_DedupsSlackByChannelAndTS(t *testing.T) {
	// The same Slack event can be delivered twice over one socket when it's
	// visible to both the bot and the authorizing user (the user-scoped event
	// subscriptions overlap the bot's), and reconnects/backfill can replay it.
	// (channel, ts) uniquely identifies a Slack message, so the second copy
	// must be a no-op rather than a duplicate inbox entry / double wake.
	slug := inboxTestSlug(t)
	ev := InboundEvent{
		Kind:        "message",
		Channel:     "D_ALICE",
		ChannelType: "im",
		TS:          "1700000100.000001",
		ThreadTS:    "1700000100.000001",
		UserID:      "U_alice",
		Text:        "thanks!",
	}
	for i := 0; i < 2; i++ {
		if err := AppendInboxEvent(slug, ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("duplicate (channel, ts) should append once; len = %d", len(entries))
	}
	if entries[0].Event.Text != "thanks!" {
		t.Errorf("kept entry wrong: %+v", entries[0].Event)
	}
}

func TestAppendInboxEventStamped_RoundTripsAutoPermitMeta(t *testing.T) {
	// The auto-permit stamp (calibrated confidence + trusted-source) must survive
	// the JSONL round-trip so the unattended wake gate can read it. A plain
	// AppendInboxEvent must leave both zero (fail-closed default).
	slug := inboxTestSlug(t)
	stamped := InboundEvent{Kind: "message", Channel: "C_TEAM", ChannelType: "slack", TS: "1700000300.000003", ThreadTS: "1700000300.000003", UserID: "U_self", Text: "stamped"}
	if err := AppendInboxEventStamped(slug, stamped, 0.93, true); err != nil {
		t.Fatalf("append stamped: %v", err)
	}
	plain := InboundEvent{Kind: "message", Channel: "C_TEAM", ChannelType: "slack", TS: "1700000400.000004", ThreadTS: "1700000400.000004", UserID: "U_x", Text: "plain"}
	if err := AppendInboxEvent(slug, plain); err != nil {
		t.Fatalf("append plain: %v", err)
	}
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if got := entries[0].Meta; !got.TrustedSource || got.CalibratedConfidence != 0.93 || got.Source != "slack" {
		t.Errorf("stamped meta = %+v, want trusted=true conf=0.93 source=slack", got)
	}
	if got := entries[1].Meta; got.TrustedSource || got.CalibratedConfidence != 0 {
		t.Errorf("plain meta = %+v, want trusted=false conf=0 (fail-closed)", got)
	}
}

func TestAppendInboxEvent_DistinctSlackTSBothAppend(t *testing.T) {
	// Dedup must key on ts, not just channel — two different messages in the
	// same DM channel are distinct events and both belong in the inbox.
	slug := inboxTestSlug(t)
	for _, ts := range []string{"1700000100.000001", "1700000200.000002"} {
		ev := InboundEvent{Kind: "message", Channel: "D_ALICE", ChannelType: "im", TS: ts, ThreadTS: ts, UserID: "U_alice", Text: "msg-" + ts}
		if err := AppendInboxEvent(slug, ev); err != nil {
			t.Fatalf("append %s: %v", ts, err)
		}
	}
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("distinct ts should both append; len = %d", len(entries))
	}
}

func TestAppendInboxEvent_DoesNotDedupGitHub(t *testing.T) {
	// GitHub events carry Channel=repo and TS=updated_at, so two distinct
	// events on one repo can share a timestamp-second without being
	// duplicates. Dedup is Slack-only; GitHub events must never be collapsed.
	slug := inboxTestSlug(t)
	for _, kind := range []string{"pr_comment", "pr_review"} {
		ev := InboundEvent{
			Kind:        kind,
			Channel:     "acme/app",
			ChannelType: "github",
			TS:          "2026-06-03T10:00:00Z", // identical updated_at
			UserID:      "octocat",
			Text:        kind + " body",
		}
		if err := AppendInboxEvent(slug, ev); err != nil {
			t.Fatalf("append %s: %v", kind, err)
		}
	}
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("GitHub events sharing (channel, ts) must not dedup; len = %d", len(entries))
	}
}

func TestAppendInboxEvent_RoundtripPreservesFields(t *testing.T) {
	slug := inboxTestSlug(t)
	ev := InboundEvent{
		Kind:        "reaction_added",
		Channel:     "C42",
		ChannelType: "channel",
		TS:          "1234.0010",
		ThreadTS:    "1234.0001",
		UserID:      "U_me",
		Reaction:    "claude",
		ItemChannel: "C42",
		ItemTS:      "1234.0001",
		ItemAuthor:  "U_other",
		TeamID:      "T1",
		APIAppID:    "A1",
		URL:         "https://github.com/acme/app/pull/12#discussion_r1",
		RawJSON:     `{"some":"raw"}`,
	}
	if err := AppendInboxEvent(slug, ev); err != nil {
		t.Fatalf("append err = %v", err)
	}
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d", len(entries))
	}
	got := entries[0].Event
	if got.Reaction != "claude" || got.ItemAuthor != "U_other" || got.ChannelType != "channel" {
		t.Errorf("roundtrip lost fields: %+v", got)
	}
	if got.RawJSON != `{"some":"raw"}` {
		t.Errorf("RawJSON lost: %q", got.RawJSON)
	}
	if got.URL != "https://github.com/acme/app/pull/12#discussion_r1" {
		t.Errorf("URL lost: %q", got.URL)
	}
}

func TestAppendInboxEvent_AddsClassifiedMeta(t *testing.T) {
	slug := inboxTestSlug(t)
	ev := InboundEvent{
		Kind:        "pr_review_comment",
		ChannelType: "github",
		Text:        "please rename this helper",
		URL:         "https://github.com/acme/app/pull/12#discussion_r1",
	}
	if err := AppendInboxEvent(slug, ev); err != nil {
		t.Fatalf("AppendInboxEvent() error = %v", err)
	}

	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("ReadInboxEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(entries))
	}
	if entries[0].Meta.Source != "github" {
		t.Fatalf("source = %q, want github", entries[0].Meta.Source)
	}
	if !entries[0].Meta.Actionable {
		t.Fatalf("actionable = false, want true")
	}
	if entries[0].Event.URL != ev.URL {
		t.Fatalf("url = %q, want %q", entries[0].Event.URL, ev.URL)
	}
}

func TestClassifyInboxEvent_GitHubLifecycleEventsAreActionable(t *testing.T) {
	// Every GitHub PR/issue event must wake the live session — including the
	// lifecycle events (merge, close, approval, assignment) that were
	// previously informational-only and silently never woke the agent.
	for _, kind := range []string{
		"pr_merged", "pr_closed", "pr_review_approved",
		"pr_assigned", "pr_review_requested", "pr_head_updated",
		"pr_review_comment", "pr_comment", "issue_comment", "issue_assigned",
	} {
		meta := ClassifyInboxEvent(InboundEvent{Kind: kind, ChannelType: "github"})
		if meta.Source != "github" {
			t.Errorf("kind %q: source = %q, want github", kind, meta.Source)
		}
		if !meta.Actionable {
			t.Errorf("kind %q: Actionable = false, want true (every GitHub event should wake the session)", kind)
		}
	}
}

func TestClassifyInboxEvent_FlowTellIsActionableButNoticeIsNot(t *testing.T) {
	tell := ClassifyInboxEvent(InboundEvent{Kind: "flow_tell", ChannelType: "flow"})
	if tell.Source != "flow" || !tell.Actionable {
		t.Fatalf("flow_tell meta = %+v, want actionable flow", tell)
	}
	notice := ClassifyInboxEvent(InboundEvent{Kind: "flow_notice", ChannelType: "flow"})
	if notice.Source != "flow" || notice.Actionable {
		t.Fatalf("flow_notice meta = %+v, want non-actionable flow", notice)
	}
}

func TestClassifyInboxEvent_AttentionForwardIsActionableSlack(t *testing.T) {
	meta := ClassifyInboxEvent(InboundEvent{Kind: "attention_forward", ChannelType: "slack"})
	if meta.Source != "slack" || !meta.Actionable {
		t.Fatalf("attention_forward meta = %+v, want actionable slack", meta)
	}
}

func TestReadInboxEntries_AcceptsLegacyRowsWithoutMeta(t *testing.T) {
	slug := inboxTestSlug(t)
	if err := os.MkdirAll(TaskDir(slug), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	legacy := `{"enqueued_at":"2026-05-23T10:00:00Z","event":{"kind":"message","channel_type":"slack","text":"ping"}}` + "\n"
	if err := os.WriteFile(InboxPath(slug), []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("ReadInboxEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(entries))
	}
	if entries[0].Meta.Source != "" {
		t.Fatalf("legacy source = %q, want empty", entries[0].Meta.Source)
	}
}

func TestReadInboxEntries_MissingFileReturnsEmpty(t *testing.T) {
	slug := inboxTestSlug(t)
	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("err = %v, want nil for missing file", err)
	}
	if len(entries) != 0 {
		t.Errorf("len = %d, want 0", len(entries))
	}
}

func TestReadInboxEntries_SkipsMalformedLines(t *testing.T) {
	slug := inboxTestSlug(t)
	// Hand-write a file with a good line, a garbage line, an empty line, and another good line.
	good := InboxEntry{EnqueuedAt: "2026-01-01T00:00:00Z", Event: InboundEvent{Kind: "message", Text: "ok"}}
	good2 := InboxEntry{EnqueuedAt: "2026-01-01T00:00:01Z", Event: InboundEvent{Kind: "message", Text: "fine"}}
	b1, _ := json.Marshal(good)
	b2, _ := json.Marshal(good2)
	content := string(b1) + "\n{garbage not json\n\n" + string(b2) + "\n"

	if err := os.MkdirAll(TaskDir(slug), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(InboxPath(slug), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadInboxEntries(slug)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2 (garbage skipped, empty skipped)", len(entries))
	}
	if entries[0].Event.Text != "ok" || entries[1].Event.Text != "fine" {
		t.Errorf("got = %+v", entries)
	}
}

func TestInboxCursor_Roundtrip(t *testing.T) {
	slug := inboxTestSlug(t)
	if got, err := ReadInboxCursor(slug); err != nil || got != "" {
		t.Fatalf("missing cursor: got %q err %v, want empty", got, err)
	}
	if err := WriteInboxCursor(slug, "1234.0050"); err != nil {
		t.Fatalf("write cursor: %v", err)
	}
	got, err := ReadInboxCursor(slug)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if got != "1234.0050" {
		t.Errorf("cursor = %q, want 1234.0050", got)
	}
	// Overwrite — atomic rename should replace cleanly.
	if err := WriteInboxCursor(slug, "9999.9999"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = ReadInboxCursor(slug)
	if got != "9999.9999" {
		t.Errorf("overwritten cursor = %q", got)
	}
}

func TestInboxMonitorCursor_IsSeparateFromSlackCursor(t *testing.T) {
	slug := inboxTestSlug(t)
	if err := WriteInboxCursor(slug, "1716460000.000100"); err != nil {
		t.Fatalf("WriteInboxCursor() error = %v", err)
	}
	if err := WriteInboxMonitorCursor(slug, 64); err != nil {
		t.Fatalf("WriteInboxMonitorCursor() error = %v", err)
	}

	slackCursor, err := ReadInboxCursor(slug)
	if err != nil {
		t.Fatalf("ReadInboxCursor() error = %v", err)
	}
	monitorCursor, err := ReadInboxMonitorCursor(slug)
	if err != nil {
		t.Fatalf("ReadInboxMonitorCursor() error = %v", err)
	}
	if slackCursor != "1716460000.000100" {
		t.Fatalf("slack cursor = %q", slackCursor)
	}
	if monitorCursor != 64 {
		t.Fatalf("monitor cursor = %d, want 64", monitorCursor)
	}
}

func TestInboxPaths_NoFlowRootAndNoHomeFails(t *testing.T) {
	// When neither FLOW_ROOT nor HOME resolves, paths should empty and
	// downstream writes should error rather than write to "" silently.
	t.Setenv("FLOW_ROOT", "")
	t.Setenv("HOME", "")
	if got := TaskDir("anything"); got != "" {
		// Some platforms may still resolve HOME via getpwuid; skip if so.
		t.Skip("home still resolvable on this platform")
	}
	if err := AppendInboxEvent("anything", InboundEvent{Kind: "message"}); err == nil {
		t.Errorf("expected error when no paths resolvable")
	}
}
