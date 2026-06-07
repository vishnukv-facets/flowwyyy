// internal/steering/cascade_test.go
package steering

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flow/internal/flowdb"
	"flow/internal/monitor"
)

// captureTraces wires c.trace to append into a slice and returns a pointer to
// it, so a test can assert on the decision-trace rows the cascade emits.
func captureTraces(c *Cascade) *[]flowdb.SteeringTrace {
	var traces []flowdb.SteeringTrace
	c.trace = func(t flowdb.SteeringTrace) { traces = append(traces, t) }
	return &traces
}

func cascadeFixture(t *testing.T) (*Cascade, *sql.DB) {
	t.Helper()
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	c := NewCascade(db, WatchConfig{
		WatchedChannels: map[string]bool{"C1": true},
		Identity:        OperatorIdentity{UserIDs: []string{"U_ME"}},
		MentionUserIDs:  []string{"U_ME"},
	})
	// deterministic id + clock for assertions
	n := 0
	c.newID = func() string { n++; return "id" + string(rune('0'+n)) }
	c.now = func() time.Time { return time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC) }
	return c, db
}

func msg(channel, ts, user, text string) monitor.InboundEvent {
	return monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: channel, TS: ts, ThreadTS: ts, UserID: user, Text: text}
}

func stage1JSONForPrompt(prompt string, relevant bool, reason string) string {
	tail := prompt
	if marker := strings.LastIndex(prompt, "Events (JSON array):"); marker >= 0 {
		tail = prompt[marker:]
	}
	jsonText, _ := extractJSON(tail)
	var inputs []ClassifyInput
	_ = json.Unmarshal([]byte(jsonText), &inputs)
	out := make([]RelevanceVerdict, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, RelevanceVerdict{ThreadKey: in.ThreadKey, Relevant: relevant, Reason: reason})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func TestObserveTraceGitHubSource(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return stage1JSONForPrompt(prompt, false, "looks like a meta note"), nil
		}
		return `{"suggested_action":"drop","confidence":0.8,"reason":"no operator action required"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.9,"reason":"no operator action required"}`, nil
	})
	ev := monitor.InboundEvent{
		Kind: "pr_comment", ChannelType: "github", Channel: "o/r",
		TS: "2026-06-05T10:00:00Z", ThreadTS: "gh-pr:o/r#5",
		UserID: "reviewer", Text: "please review",
		URL: "https://github.com/o/r/pull/5",
	}
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want exactly 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.Source != "github" {
		t.Errorf("trace Source = %q, want %q", tr.Source, "github")
	}
	if tr.URL != "https://github.com/o/r/pull/5" {
		t.Errorf("trace URL = %q, want the GitHub url", tr.URL)
	}
	// It cleared Stage 0 and advisory Stage 1, then the deep agent made the final drop call.
	if tr.StageReached != "stage3" {
		t.Errorf("StageReached = %q, want stage3 (stage1 is advisory)", tr.StageReached)
	}
	if tr.Stage1Relevant == nil || *tr.Stage1Relevant {
		t.Errorf("Stage1Relevant = %v, want false recorded as advisory", tr.Stage1Relevant)
	}
	if tr.Stage1Reason != "looks like a meta note" {
		t.Errorf("Stage1Reason = %q", tr.Stage1Reason)
	}
}

func TestCascadeSurfacesSurvivor(t *testing.T) {
	c, db := cascadeFixture(t)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:1.1","relevant":true,"urgency_hint":"urgent"}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8,"summary":"q"}`, nil // stage2
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"reply","confidence":0.9,"summary":"customer q","draft":"On it."}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "1.1", "U_OTHER", "need help")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	if items[0].Draft != "On it." || items[0].SuggestedAction != "reply" || items[0].ThreadKey != "C1:1.1" {
		t.Errorf("feed item = %+v", items[0])
	}
}

func TestCascadeFeedCapturesSourceContext(t *testing.T) {
	c, db := cascadeFixture(t)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"D30:30.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"reply","confidence":0.9,"summary":"q","draft":"On it."}`, nil
	})
	ev := monitor.InboundEvent{
		Kind: "message", ChannelType: "im", Channel: "D30", TS: "30.1", ThreadTS: "30.1",
		UserID: "U_BOB", Text: "need help", TeamID: "T_WS",
		URL: "https://example.slack.com/archives/D30/p301",
	}
	// Stage0 watches C1 by default; an im DM must clear Stage 0. Override config
	// to watch this DM channel so the event survives to the feed write.
	c.Config.WatchedChannels = map[string]bool{"C1": true, "D30": true}
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	it := items[0]
	if it.Channel != "D30" || it.ChannelType != "im" || it.Author != "U_BOB" ||
		it.TS != "30.1" || it.TeamID != "T_WS" || it.URL != "https://example.slack.com/archives/D30/p301" {
		t.Errorf("feed row missing source context: %+v", it)
	}
}

func TestCascadeFetchesContextForDeepTriageAndStoresJSON(t *testing.T) {
	c, db := cascadeFixture(t)
	c.FetchContext = func(_ context.Context, ev monitor.InboundEvent) (ThreadContext, error) {
		return ThreadContext{
			Source:      "slack",
			ThreadKey:   monitor.ThreadKey(ev.Channel, ev.ThreadTS),
			Permalink:   "https://example.slack.com/archives/C1/p111",
			FetchStatus: "ok",
			Parent: &ContextMessage{
				Kind:   "parent",
				Author: "alice",
				Text:   "Need ETA on the migration",
				TS:     "1.1",
			},
			Messages: []ContextMessage{{
				Kind:   "reply",
				Author: "bob",
				Text:   "Can we ship Friday?",
				TS:     "1.2",
			}},
			Participants: []string{"alice", "bob"},
			Timestamps:   []string{"1.1", "1.2"},
			Summary:      "2 Slack messages from alice, bob",
		}, nil
	}
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:1.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		if !strings.Contains(prompt, "Need ETA on the migration") || !strings.Contains(prompt, "Can we ship Friday?") {
			t.Fatalf("deep triage prompt missing fetched context:\n%s", prompt)
		}
		return `{"suggested_action":"reply","confidence":0.9,"summary":"migration ETA","draft":"Targeting Friday."}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "1.1", "U_OTHER", "need help")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	var stored ThreadContext
	if err := json.Unmarshal([]byte(items[0].ContextJSON), &stored); err != nil {
		t.Fatalf("context_json is not a ThreadContext JSON object: %v\n%s", err, items[0].ContextJSON)
	}
	if stored.Permalink != "https://example.slack.com/archives/C1/p111" ||
		stored.Parent == nil || stored.Parent.Text != "Need ETA on the migration" ||
		len(stored.Messages) != 1 || stored.Messages[0].Text != "Can we ship Friday?" ||
		stored.Summary != "2 Slack messages from alice, bob" {
		t.Errorf("stored context mismatch: %+v", stored)
	}
}

func TestCascadeBuildsTaskImpactHintsWithResolvedAuthorName(t *testing.T) {
	c, db := cascadeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO tasks (slug,name,status,kind,priority,work_dir,waiting_on,session_provider,created_at,updated_at)
		 VALUES ('raptor-review','Raptor PR review','in-progress','regular','high','/tmp','Rohit review on PR #159','codex',?,?)`,
		now, now,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	c.ResolveUserName = func(_ context.Context, id string) string {
		if id == "U_ROHIT" {
			return "Rohit Raveendran"
		}
		return ""
	}
	c.FetchContext = func(_ context.Context, ev monitor.InboundEvent) (ThreadContext, error) {
		return ThreadContext{
			Source:      "slack",
			ThreadKey:   monitor.ThreadKey(ev.Channel, ev.ThreadTS),
			FetchStatus: "ok",
			Parent: &ContextMessage{
				Kind:   "parent",
				Author: ev.UserID,
				Text:   ev.Text,
				TS:     ev.TS,
			},
			Participants: []string{ev.UserID},
		}, nil
	}
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:44.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"digest_only","confidence":0.8,"summary":"leave FYI"}`, nil
	})
	var deepPrompt string
	stubDeepTriage(t, func(prompt string) (string, error) {
		deepPrompt = prompt
		return `{"suggested_action":"forward","matched_task":"raptor-review","confidence":0.9,"summary":"Rohit is unavailable","reason":"Rohit affects the active review task"}`, nil
	})

	ev := msg("C1", "44.1", "U_ROHIT", "I'll be on leave tomorrow and the day after.")
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	if !strings.Contains(deepPrompt, `"task_slug":"raptor-review"`) {
		t.Fatalf("deep prompt missing task-impact hint:\n%s", deepPrompt)
	}
	if !strings.Contains(deepPrompt, "Rohit Raveendran") {
		t.Fatalf("deep prompt missing resolved display name:\n%s", deepPrompt)
	}
	if strings.Contains(deepPrompt, "waiting_on mentions U_ROHIT") {
		t.Fatalf("deep prompt used raw Slack id as hint person:\n%s", deepPrompt)
	}
}

func TestCascadeStoresFallbackContextWhenFetchFails(t *testing.T) {
	c, db := cascadeFixture(t)
	c.FetchContext = func(context.Context, monitor.InboundEvent) (ThreadContext, error) {
		return ThreadContext{}, errTestContextFetch
	}
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:1.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		if !strings.Contains(prompt, `"fetch_status":"error"`) || !strings.Contains(prompt, "fallback from event") {
			t.Fatalf("deep prompt missing fallback context:\n%s", prompt)
		}
		return `{"suggested_action":"reply","confidence":0.9,"summary":"fallback","draft":"On it."}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "1.1", "U_OTHER", "fallback from event")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	var stored ThreadContext
	if err := json.Unmarshal([]byte(items[0].ContextJSON), &stored); err != nil {
		t.Fatalf("context_json invalid: %v", err)
	}
	if stored.FetchStatus != "error" || stored.FetchError == "" || stored.Parent == nil || stored.Parent.Text != "fallback from event" {
		t.Errorf("fallback context mismatch: %+v", stored)
	}
}

func TestCascadeScrubsInternalFetchDetailsBeforeWritingFeed(t *testing.T) {
	c, db := cascadeFixture(t)
	c.FetchContext = func(context.Context, monitor.InboundEvent) (ThreadContext, error) {
		return ThreadContext{}, errors.New("slack context fetch: not_in_channel")
	}
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:1.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"digest_only","confidence":0.8,"summary":"leave notice"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"digest_only","confidence":0.82,"summary":"Teammate announced leave","reason":"Slack context fetch failed (not_in_channel) so the sender's name couldn't be resolved, but the message text is self-contained and clearly FYI-only. Worth surfacing in a digest so the operator knows the person is unavailable."}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "1.1", "U_OTHER", "I'll be on leave tomorrow and the day after.")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	reason := items[0].Reason
	for _, leak := range []string{"Slack context fetch failed", "not_in_channel", "sender's name couldn't be resolved"} {
		if strings.Contains(reason, leak) {
			t.Fatalf("feed reason leaked %q: %q", leak, reason)
		}
	}
	if !strings.Contains(reason, "message text is self-contained") || !strings.Contains(reason, "Worth surfacing in a digest") {
		t.Fatalf("feed reason lost the useful decision rationale: %q", reason)
	}
}

func TestCascadeStage0DropWritesNothing(t *testing.T) {
	c, db := cascadeFixture(t)
	// self-authored → Stage0 drops before any model call
	if err := c.Observe(context.Background(), msg("C1", "2.1", "U_ME", "note")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, ""); len(items) != 0 {
		t.Errorf("expected no feed items, got %d", len(items))
	}
}

func TestCascadeStage1FalseStillReachesDeepTriage(t *testing.T) {
	c, db := cascadeFixture(t)
	traces := captureTraces(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:3.1","relevant":false,"reason":"cheap gate thinks this is casual"}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8,"summary":"needs deeper look"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.91,"reason":"deep read found no operator action"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "3.1", "U_OTHER", "lol")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, ""); len(items) != 0 {
		t.Errorf("expected no feed items, got %d", len(items))
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.StageReached != "stage3" || tr.Stage1Relevant == nil || *tr.Stage1Relevant {
		t.Fatalf("trace = %+v, want advisory stage1=false then stage3 drop", tr)
	}
	if tr.Stage1Reason != "cheap gate thinks this is casual" {
		t.Fatalf("Stage1Reason = %q", tr.Stage1Reason)
	}
	if !strings.Contains(tr.DropReason, "deep read found no operator action") {
		t.Fatalf("DropReason = %q, want deep reason", tr.DropReason)
	}
}

// A deep-triage 'drop' verdict must NOT become a feed card — it's cascade-
// classified noise. It still records a dropped trace for transparency.
func TestCascadeDeepDropNotSurfaced(t *testing.T) {
	c, db := cascadeFixture(t)
	traces := captureTraces(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:7.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.7,"summary":"q"}`, nil // stage2 passes
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.85,"summary":"bot noise, not for operator","reason":"automation status only"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "7.1", "U_OTHER", "create+close dashboard task")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 0 {
		t.Errorf("drop verdict must not surface a feed card, got %d", len(items))
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.Disposition != "dropped" {
		t.Errorf("trace disposition = %q, want dropped", tr.Disposition)
	}
	if tr.StageReached != "stage3" {
		t.Errorf("trace stage = %q, want stage3", tr.StageReached)
	}
	if tr.FeedItemID != "" {
		t.Errorf("dropped trace must not record a FeedItemID, got %q", tr.FeedItemID)
	}
	if !strings.Contains(tr.DropReason, "automation status only") {
		t.Errorf("DropReason = %q, want deep reason", tr.DropReason)
	}
}

func TestCascadeVerdictCacheSkipsRepeat(t *testing.T) {
	c, db := cascadeFixture(t)
	calls := 0
	stubClassifier(t, func(prompt string) (string, error) {
		calls++
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:4.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"reply","confidence":0.9,"summary":"q"}`, nil
	})
	ev := msg("C1", "4.1", "U_OTHER", "help")
	_ = c.Observe(context.Background(), ev)
	callsAfterFirst := calls
	_ = c.Observe(context.Background(), ev) // same thread within TTL
	if calls != callsAfterFirst {
		t.Errorf("second Observe should hit verdict cache and make no model calls (calls %d -> %d)", callsAfterFirst, calls)
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 1 {
		t.Errorf("cache must prevent a duplicate feed row, got %d", len(items))
	}
}

func TestCascadeBudgetExhaustionSurfacesStage2(t *testing.T) {
	c, db := cascadeFixture(t)
	c.budget = newBudgetGuard(0) // zero deep-triage budget
	deepCalled := false
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:5.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.7,"summary":"stage2 only"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) { deepCalled = true; return "{}", nil })
	if err := c.Observe(context.Background(), msg("C1", "5.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if deepCalled {
		t.Error("deep triage must NOT run when budget is exhausted")
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 || items[0].Summary != "stage2 only" {
		t.Errorf("budget exhaustion must still surface the stage2 verdict, got %+v", items)
	}
}

func TestObserveTraceStage0Drop(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	// self-authored (U_ME is in cfg.Identity.UserIDs) → Stage0 drop, no model call
	if err := c.Observe(context.Background(), msg("C1", "10.1", "U_ME", "note")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want exactly 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.Disposition != "dropped" || tr.StageReached != "stage0" || tr.DropReason != "self-authored" {
		t.Errorf("trace = %+v; want dropped/stage0/self-authored", tr)
	}
}

func TestObserveTraceSurfaced(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:11.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.72,"summary":"do it"}`, nil // stage2
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"deep","draft":""}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "11.1", "U_OTHER", "please do this")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want exactly 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.Disposition != "surfaced" || tr.StageReached != "stage3" {
		t.Errorf("trace disposition/stage = %s/%s; want surfaced/stage3", tr.Disposition, tr.StageReached)
	}
	if tr.FeedItemID == "" {
		t.Error("surfaced trace must record a FeedItemID")
	}
	if tr.FinalAction != "make_task" {
		t.Errorf("FinalAction = %q, want make_task", tr.FinalAction)
	}
	if tr.Stage1Relevant == nil || !*tr.Stage1Relevant {
		t.Errorf("Stage1Relevant = %v, want non-nil true", tr.Stage1Relevant)
	}
}

func TestObserveTraceCacheDuplicate(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:12.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"reply","confidence":0.8}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"reply","confidence":0.9,"summary":"q"}`, nil
	})
	ev := msg("C1", "12.1", "U_OTHER", "help")
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	if err := c.Observe(context.Background(), ev); err != nil { // same thread within TTL
		t.Fatalf("second Observe: %v", err)
	}
	if len(*traces) != 2 {
		t.Fatalf("trace count = %d, want 2 (one per Observe)", len(*traces))
	}
	second := (*traces)[1]
	if second.Disposition != "dropped" || second.StageReached != "cache" {
		t.Errorf("second trace = %s/%s; want dropped/cache", second.Disposition, second.StageReached)
	}
}

func TestCascadeGitHubReviewCommentsDoNotShareThreadVerdictCache(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	stage1Calls := 0
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			stage1Calls++
			return stage1JSONForPrompt(prompt, false, "cheap gate thinks it is informational"), nil
		}
		return `{"suggested_action":"drop","confidence":0.8,"reason":"no operator action required"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.9,"reason":"no operator action required"}`, nil
	})

	first := monitor.InboundEvent{
		Kind: "pr_review_comment", ChannelType: "github", Channel: "o/r",
		TS: "2026-06-07T09:58:19Z", ThreadTS: "gh-pr:o/r#159",
		UserID: "reviewer", Text: "please restore stderr",
		URL: "https://github.com/o/r/pull/159#discussion_r1",
	}
	second := first
	second.TS = "2026-06-07T09:58:30Z"
	second.Text = "please validate archive paths"
	second.URL = "https://github.com/o/r/pull/159#discussion_r2"

	if err := c.Observe(context.Background(), first); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	if err := c.Observe(context.Background(), second); err != nil {
		t.Fatalf("second Observe: %v", err)
	}
	if stage1Calls != 2 {
		t.Fatalf("stage1 calls = %d, want 2; distinct GitHub comments must not share a PR-thread verdict cache", stage1Calls)
	}
	if len(*traces) != 2 {
		t.Fatalf("trace count = %d, want 2", len(*traces))
	}
	if (*traces)[1].StageReached == "cache" {
		t.Fatalf("second GitHub review comment was dropped by thread cache: %+v", (*traces)[1])
	}
	if (*traces)[1].StageReached != "stage3" || (*traces)[1].Stage1Relevant == nil || *(*traces)[1].Stage1Relevant {
		t.Fatalf("second trace = %+v, want advisory stage1=false and final stage3 decision", (*traces)[1])
	}
}

func TestCascadeSlackRepliesDoNotShareThreadVerdictCache(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	stage1Calls := 0
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			stage1Calls++
			return stage1JSONForPrompt(prompt, false, "cheap gate thinks it is casual"), nil
		}
		return `{"suggested_action":"drop","confidence":0.8,"reason":"no operator action required"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.9,"reason":"no operator action required"}`, nil
	})

	first := monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C1", TS: "10.1", ThreadTS: "10.0", UserID: "U_OTHER", Text: "first ambiguous reply"}
	second := first
	second.TS = "10.2"
	second.Text = "second ambiguous reply"

	if err := c.Observe(context.Background(), first); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	if err := c.Observe(context.Background(), second); err != nil {
		t.Fatalf("second Observe: %v", err)
	}
	if stage1Calls != 2 {
		t.Fatalf("stage1 calls = %d, want 2; distinct Slack replies must not share a thread verdict cache", stage1Calls)
	}
	if len(*traces) != 2 {
		t.Fatalf("trace count = %d, want 2", len(*traces))
	}
	if (*traces)[1].StageReached == "cache" {
		t.Fatalf("second Slack reply was dropped by thread cache: %+v", (*traces)[1])
	}
	if (*traces)[1].StageReached != "stage3" || (*traces)[1].Stage1Relevant == nil || *(*traces)[1].Stage1Relevant {
		t.Fatalf("second trace = %+v, want advisory stage1=false and final stage3 decision", (*traces)[1])
	}
}

func TestCascadeCodeRabbitPotentialIssueBypassesStage1Drop(t *testing.T) {
	c, db := cascadeFixture(t)
	traces := captureTraces(c)
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "raptor-airgapped", true }
	t.Cleanup(func() { matchExistingTask = old })

	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"facets-cloud/raptor:gh-pr:facets-cloud/raptor#159","relevant":false}]`, nil
		}
		return `{"suggested_action":"forward","matched_task":"raptor-airgapped","confidence":0.84,"summary":"CodeRabbit flagged a critical review comment"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"forward","matched_task":"raptor-airgapped","confidence":0.92,"summary":"CodeRabbit critical finding needs task follow-up","reason":"review bot marked it as a potential issue"}`, nil
	})

	ev := monitor.InboundEvent{
		Kind: "pr_review_comment", ChannelType: "github", Channel: "facets-cloud/raptor",
		TS: "2026-06-07T09:58:19Z", ThreadTS: "gh-pr:facets-cloud/raptor#159",
		UserID: "coderabbitai[bot]",
		Text:   "_Potential issue_ | _Critical_ | _Quick win_\n\n**Restore stderr to stderr for validation failures.**",
		URL:    "https://github.com/facets-cloud/raptor/pull/159#discussion_rcritical",
	}
	if err := c.Observe(context.Background(), ev); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1 actionable CodeRabbit item", len(items))
	}
	if items[0].MatchedTask != "raptor-airgapped" || items[0].SuggestedAction != "forward" {
		t.Fatalf("feed item = %+v, want forward to matched task", items[0])
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.Disposition != "surfaced" {
		t.Fatalf("trace disposition = %q, want surfaced: %+v", tr.Disposition, tr)
	}
	if tr.Stage1Relevant == nil || !*tr.Stage1Relevant {
		t.Fatalf("Stage1Relevant = %v, want true for actionable CodeRabbit finding", tr.Stage1Relevant)
	}
}

// stage1RelevanceCalls counts how many classifier prompts contained the
// stage1-relevance mode marker (reset per test).
var stage1RelevanceCalls int

func TestObserveBatchSingleStage1Call(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	stage1RelevanceCalls = 0
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			stage1RelevanceCalls++
			// Echo every input thread_key back as relevant so Stage1Relevance
			// (which matches by key and fails closed) blesses both survivors.
			i := strings.Index(prompt, "[")
			j := strings.LastIndex(prompt, "]")
			var inputs []ClassifyInput
			if i >= 0 && j > i {
				_ = json.Unmarshal([]byte(prompt[i:j+1]), &inputs)
			}
			out := make([]RelevanceVerdict, 0, len(inputs))
			for _, in := range inputs {
				out = append(out, RelevanceVerdict{ThreadKey: in.ThreadKey, Relevant: true})
			}
			b, _ := json.Marshal(out)
			return string(b), nil
		}
		return `{"suggested_action":"reply","confidence":0.8,"summary":"q"}`, nil // stage2
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"reply","confidence":0.9,"summary":"q"}`, nil
	})
	evs := []monitor.InboundEvent{
		msg("C1", "20.1", "U_ME", "self note"), // stage0 drop
		msg("C1", "21.1", "U_OTHER", "need a hand"),
		msg("C1", "22.1", "U_OTHER", "another one"),
	}
	if err := c.ObserveBatch(context.Background(), evs); err != nil {
		t.Fatalf("ObserveBatch: %v", err)
	}
	if stage1RelevanceCalls != 1 {
		t.Errorf("stage1-relevance call count = %d, want exactly 1 (one batched call)", stage1RelevanceCalls)
	}
	if len(*traces) != 3 {
		t.Errorf("trace count = %d, want 3 (one per event)", len(*traces))
	}
}

func TestCascadeTextCleanDeIDsBeforeLLMAndTrace(t *testing.T) {
	c, _ := cascadeFixture(t)
	traces := captureTraces(c)
	// A fake cleaner that rewrites the raw Slack mention markup to a name,
	// mimicking SlackNameResolver.CleanText without a Slack token.
	c.TextClean = func(_ context.Context, text string) string {
		return strings.ReplaceAll(text, "<@U1>", "@alice")
	}
	var stage1Prompt string
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			stage1Prompt = prompt
			return `[{"thread_key":"C1:1.1","relevant":false}]`, nil // drop cheaply after stage1
		}
		return `{"suggested_action":"reply","confidence":0.8}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "1.1", "U_OTHER", "hi <@U1>")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// The classifier must have received the CLEANED text (no raw <@U…>).
	if !strings.Contains(stage1Prompt, "@alice") {
		t.Errorf("stage1 prompt should carry the cleaned mention, got:\n%s", stage1Prompt)
	}
	if strings.Contains(stage1Prompt, "<@U1>") {
		t.Errorf("stage1 prompt must NOT carry the raw mention markup, got:\n%s", stage1Prompt)
	}
	// And the trace preview must store the cleaned text too.
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	tr := (*traces)[0]
	if !strings.Contains(tr.TextPreview, "@alice") {
		t.Errorf("trace TextPreview should carry cleaned text, got %q", tr.TextPreview)
	}
	if strings.Contains(tr.TextPreview, "<@U1>") {
		t.Errorf("trace TextPreview must NOT carry raw mention markup, got %q", tr.TextPreview)
	}
}

func TestCascadeTextCleanNilIsIdentity(t *testing.T) {
	c, _ := cascadeFixture(t) // TextClean unset
	traces := captureTraces(c)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return stage1JSONForPrompt(prompt, false, "test advisory drop"), nil
		}
		return `{"suggested_action":"drop","confidence":0.8,"reason":"test drop"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.9,"reason":"test drop"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "2.1", "U_OTHER", "raw <@U1> stays")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	if (*traces)[0].TextPreview != "raw <@U1> stays" {
		t.Errorf("nil TextClean must pass text through unchanged, got %q", (*traces)[0].TextPreview)
	}
}

func TestCascadeMatchExistingTaskRewritesMakeTask(t *testing.T) {
	c, db := cascadeFixture(t)
	// Stub the matcher so we don't depend on real task seeding here — the
	// matcher's own tag/connector logic is exercised separately.
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "gh-pr-task", true }
	t.Cleanup(func() { matchExistingTask = old })

	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:90.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.8,"summary":"q"}`, nil // stage2
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"deep"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "90.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	if items[0].MatchedTask != "gh-pr-task" {
		t.Errorf("MatchedTask = %q, want gh-pr-task", items[0].MatchedTask)
	}
	if items[0].SuggestedAction != "forward" {
		t.Errorf("SuggestedAction = %q, want forward (make_task rewritten to forward)", items[0].SuggestedAction)
	}
}

func TestCascadeMatchExistingTaskBudgetExhaustedPath(t *testing.T) {
	c, db := cascadeFixture(t)
	c.budget = newBudgetGuard(0) // force the budget-exhausted (stage2) write path
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "gh-pr-task", true }
	t.Cleanup(func() { matchExistingTask = old })

	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:91.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.7,"summary":"stage2"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "91.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	if items[0].MatchedTask != "gh-pr-task" || items[0].SuggestedAction != "forward" {
		t.Errorf("budget-exhausted path: item = %+v, want MatchedTask=gh-pr-task action=forward", items[0])
	}
}

func TestCascadeNoExistingTaskLeavesActionUnchanged(t *testing.T) {
	c, db := cascadeFixture(t)
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "", false }
	t.Cleanup(func() { matchExistingTask = old })

	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:92.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.8,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"deep"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "92.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("feed len = %d, want 1", len(items))
	}
	if items[0].MatchedTask != "" {
		t.Errorf("MatchedTask = %q, want empty (no match)", items[0].MatchedTask)
	}
	if items[0].SuggestedAction != "make_task" {
		t.Errorf("SuggestedAction = %q, want make_task (unchanged)", items[0].SuggestedAction)
	}
}

// stubTaskSpawner swaps the steering taskSpawner var to count spawns instead of
// shelling out to `flow spawn`. It also stubs taskTagger (which MakeTaskFromFeed
// now calls after spawning) so the auto-act path never shells to real `flow`.
func stubTaskSpawner(t *testing.T) *int {
	t.Helper()
	calls := 0
	oldSpawn, oldTag := taskSpawner, taskTagger
	taskSpawner = func(_ context.Context, _, _, _, _ string) error { calls++; return nil }
	taskTagger = func(_ context.Context, _, _ string) error { return nil }
	t.Cleanup(func() { taskSpawner, taskTagger = oldSpawn, oldTag })
	return &calls
}

func stubFailingTaskSpawner(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	oldSpawn, oldTag := taskSpawner, taskTagger
	taskSpawner = func(_ context.Context, _, _, _, _ string) error {
		calls++
		return err
	}
	taskTagger = func(_ context.Context, _, _ string) error { return nil }
	t.Cleanup(func() { taskSpawner, taskTagger = oldSpawn, oldTag })
	return &calls
}

func TestCascadeAutoActsWhenAutonomyEnabled(t *testing.T) {
	c, db := cascadeFixture(t)
	c.Autonomy = AutonomyPolicy{ActionMakeTask: {Enabled: true, Threshold: 0.5}}
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "", false }
	t.Cleanup(func() { matchExistingTask = old })
	spawns := stubTaskSpawner(t)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:93.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.95,"summary":"deep"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "93.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if *spawns != 1 {
		t.Errorf("taskSpawner calls = %d, want 1 (auto-acted)", *spawns)
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 0 {
		t.Errorf("new feed items = %d, want 0 (auto-acted → acted)", len(items))
	}
	if items, _ := flowdb.ListFeedItems(db, "acted"); len(items) != 1 {
		t.Errorf("acted feed items = %d, want 1", len(items))
	}
}

func TestCascadeTraceRecordsAutonomyActed(t *testing.T) {
	c, _ := cascadeFixture(t)
	c.Autonomy = AutonomyPolicy{ActionMakeTask: {Enabled: true, Threshold: 0.5}}
	traces := captureTraces(c)
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "", false }
	t.Cleanup(func() { matchExistingTask = old })
	stubTaskSpawner(t)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:95.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.95,"summary":"deep"}`, nil
	})

	if err := c.Observe(context.Background(), msg("C1", "95.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(*traces) != 1 {
		t.Fatalf("trace count = %d, want 1", len(*traces))
	}
	tr := (*traces)[0]
	if tr.AutonomyAction != "make_task" || tr.AutonomyDecision != "acted" {
		t.Fatalf("autonomy trace = action %q decision %q, want make_task/acted", tr.AutonomyAction, tr.AutonomyDecision)
	}
	if !strings.Contains(tr.AutonomyReason, "confidence 0.95 >= threshold 0.50") {
		t.Errorf("autonomy reason = %q, want threshold explanation", tr.AutonomyReason)
	}
}

func TestCascadeTraceRecordsAutonomyFailureAndLeavesFeedVisible(t *testing.T) {
	c, db := cascadeFixture(t)
	c.Autonomy = AutonomyPolicy{ActionMakeTask: {Enabled: true, Threshold: 0.5}}
	traces := captureTraces(c)
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "", false }
	t.Cleanup(func() { matchExistingTask = old })
	spawns := stubFailingTaskSpawner(t, errors.New("spawn refused"))
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:96.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.9,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.95,"summary":"deep"}`, nil
	})

	if err := c.Observe(context.Background(), msg("C1", "96.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe should leave failed auto-action visible, got error: %v", err)
	}
	if *spawns != 1 {
		t.Fatalf("taskSpawner calls = %d, want 1", *spawns)
	}
	items, _ := flowdb.ListFeedItems(db, "new")
	if len(items) != 1 {
		t.Fatalf("new feed items = %d, want 1 after failed auto-action", len(items))
	}
	tr := (*traces)[0]
	if tr.AutonomyDecision != "failed" || !strings.Contains(tr.AutonomyReason, "spawn refused") {
		t.Fatalf("autonomy trace = decision %q reason %q, want failed with error", tr.AutonomyDecision, tr.AutonomyReason)
	}
}

func TestCascadeSurfaceOnlyWhenAutonomyOff(t *testing.T) {
	c, db := cascadeFixture(t) // NewCascade defaults Autonomy to all-off
	old := matchExistingTask
	matchExistingTask = func(_ *sql.DB, _ monitor.InboundEvent) (string, bool) { return "", false }
	t.Cleanup(func() { matchExistingTask = old })
	spawns := stubTaskSpawner(t)
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			return `[{"thread_key":"C1:94.1","relevant":true}]`, nil
		}
		return `{"suggested_action":"make_task","confidence":0.99,"summary":"q"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"make_task","confidence":0.99,"summary":"deep"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C1", "94.1", "U_OTHER", "please")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if *spawns != 0 {
		t.Errorf("taskSpawner calls = %d, want 0 (surface-only by default)", *spawns)
	}
	if items, _ := flowdb.ListFeedItems(db, "new"); len(items) != 1 {
		t.Errorf("new feed items = %d, want 1 (surfaced, not acted)", len(items))
	}
}

func TestMatchExistingTaskByTag(t *testing.T) {
	_, db := cascadeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	// A live task tracking a GitHub PR via the gh-pr link tag.
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,status,kind,priority,work_dir,session_provider,created_at,updated_at) VALUES ('gh-pr-550','PR 550','backlog','regular','high','/tmp','claude',?,?)`, now, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "gh-pr-550", "gh-pr:o/r#550"); err != nil {
		t.Fatalf("tag task: %v", err)
	}
	// GitHub event: the LinkTag is stashed in ThreadTS.
	ghEv := monitor.InboundEvent{
		Kind: "pr_comment", ChannelType: "github", Channel: "o/r",
		ThreadTS: "gh-pr:o/r#550", UserID: "reviewer",
	}
	if slug, ok := matchExistingTask(db, ghEv); !ok || slug != "gh-pr-550" {
		t.Errorf("matchExistingTask(github) = (%q,%v), want (gh-pr-550,true)", slug, ok)
	}

	// A Slack thread task.
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,status,kind,priority,work_dir,session_provider,created_at,updated_at) VALUES ('slack-thread-task','Slack thread','backlog','regular','high','/tmp','claude',?,?)`, now, now); err != nil {
		t.Fatalf("seed slack task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "slack-thread-task", monitor.SlackThreadTagPrefix+monitor.ThreadKey("C9", "9.1")); err != nil {
		t.Fatalf("tag slack task: %v", err)
	}
	slackEv := monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C9", ThreadTS: "9.1", UserID: "U_X"}
	if slug, ok := matchExistingTask(db, slackEv); !ok || slug != "slack-thread-task" {
		t.Errorf("matchExistingTask(slack) = (%q,%v), want (slack-thread-task,true)", slug, ok)
	}

	sharedEv := monitor.InboundEvent{
		Kind: "message", ChannelType: "im", Channel: "D_FORWARD", ThreadTS: "12.1", UserID: "U_X",
		RefChannel: "C9", RefThreadTS: "9.1", RefTS: "9.2",
	}
	if slug, ok := matchExistingTask(db, sharedEv); !ok || slug != "slack-thread-task" {
		t.Errorf("matchExistingTask(shared-ref slack) = (%q,%v), want (slack-thread-task,true)", slug, ok)
	}

	// No tracking task → (",false).
	if slug, ok := matchExistingTask(db, monitor.InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C_NONE", ThreadTS: "1.1"}); ok || slug != "" {
		t.Errorf("matchExistingTask(untracked) = (%q,%v), want (\"\",false)", slug, ok)
	}
}

// Regression: an ARCHIVED but still in-progress task must still match — archiving
// declutters the active list, it doesn't stop tracking the thread. Before the
// fix, archived tasks were invisible and the cascade suggested make_task for a PR
// that already had a (archived) task.
func TestMatchExistingTaskIncludesArchived(t *testing.T) {
	_, db := cascadeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,status,kind,priority,work_dir,session_provider,session_id,archived_at,created_at,updated_at) VALUES ('gh-pr-880','PR 880','in-progress','regular','high','/tmp','claude','sess-880',?,?,?)`, now, now, now); err != nil {
		t.Fatalf("seed archived task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "gh-pr-880", "gh-pr:o/r#880"); err != nil {
		t.Fatalf("tag task: %v", err)
	}
	ev := monitor.InboundEvent{Kind: "pr_comment", ChannelType: "github", Channel: "o/r", ThreadTS: "gh-pr:o/r#880", UserID: "reviewer"}
	if slug, ok := matchExistingTask(db, ev); !ok || slug != "gh-pr-880" {
		t.Errorf("matchExistingTask(archived) = (%q,%v), want (gh-pr-880,true)", slug, ok)
	}

	// And applyExistingTaskMatch rewrites a would-be make_task into a forward.
	c := &Cascade{DB: db}
	v := Verdict{SuggestedAction: ActionMakeTask}
	c.applyExistingTaskMatch(&v, ev)
	if v.SuggestedAction != ActionForward || v.MatchedTask != "gh-pr-880" {
		t.Errorf("after match: action=%v matched=%q, want forward / gh-pr-880", v.SuggestedAction, v.MatchedTask)
	}
}

func TestCascadeConfigFnOverridesStatic(t *testing.T) {
	c, _ := cascadeFixture(t) // static Config watches C1 (see cascadeFixture)
	// ConfigFn watches a DIFFERENT channel and mutes the static one — proves
	// Observe consults ConfigFn, not the static Config captured at construction.
	c.ConfigFn = func() WatchConfig {
		return WatchConfig{
			WatchedChannels: map[string]bool{"C_LIVE": true},
			MutedChannels:   map[string]bool{"C1": true},
		}
	}
	called := false
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			called = true
			return stage1JSONForPrompt(prompt, false, "test advisory drop"), nil
		}
		return `{"suggested_action":"drop","confidence":0.8,"reason":"test drop"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.9,"reason":"test drop"}`, nil
	})
	// Message in C_LIVE (only in ConfigFn's set, NOT the static C1 set).
	if err := c.Observe(context.Background(), msg("C_LIVE", "1.1", "U_OTHER", "hi")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !called {
		t.Error("Stage 0 should have passed using ConfigFn's watched channels (classifier never ran)")
	}

	// And a message in C1 must now drop because ConfigFn mutes it (ConfigFn wins).
	called = false
	if err := c.Observe(context.Background(), msg("C1", "2.1", "U_OTHER", "hi")); err != nil {
		t.Fatalf("Observe C1: %v", err)
	}
	if called {
		t.Error("C1 is muted in ConfigFn, so Stage 0 should drop it (classifier must not run)")
	}
}

func TestCascadeShouldObserveUsesLiveScope(t *testing.T) {
	c, _ := cascadeFixture(t)
	c.ConfigFn = func() WatchConfig {
		return WatchConfig{
			WatchedChannels: map[string]bool{"C_WATCHED": true},
			MentionUserIDs:  []string{"U_ME"},
		}
	}

	if !c.ShouldObserve(msg("C_NOISE", "1.1", "U_OTHER", "plain noise")) {
		t.Fatal("human Slack message should enter cascade even without watched-channel scope")
	}
	if !c.ShouldObserve(msg("C_WATCHED", "1.1", "U_OTHER", "watched")) {
		t.Fatal("watched channel message should enter cascade")
	}
	if !c.ShouldObserve(msg("C_NOISE", "1.2", "U_OTHER", "ping <@U_ME>")) {
		t.Fatal("operator mention in an unwatched channel should enter cascade")
	}
	if !c.ShouldObserve(monitor.InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", TS: "2.1", ThreadTS: "2.1", UserID: "U_OTHER", Text: "dm"}) {
		t.Fatal("DM should enter cascade")
	}
}

func TestCascadeConsumesLearnedSuppressionsWithoutRestart(t *testing.T) {
	t.Setenv("FLOW_STEERING_WATCH_CHANNELS", "C_NOISE")
	c, db := cascadeFixture(t)
	c.ConfigFn = WatchConfigFnWithMutes(db)

	stage1Calls := 0
	stubClassifier(t, func(prompt string) (string, error) {
		if strings.Contains(prompt, "MODE: stage1-relevance") {
			stage1Calls++
			return stage1JSONForPrompt(prompt, false, "test advisory drop"), nil
		}
		return `{"suggested_action":"drop","confidence":0.8,"reason":"test drop"}`, nil
	})
	stubDeepTriage(t, func(prompt string) (string, error) {
		return `{"suggested_action":"drop","confidence":0.9,"reason":"test drop"}`, nil
	})
	if err := c.Observe(context.Background(), msg("C_NOISE", "1.1", "U_OTHER", "first")); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	if stage1Calls != 1 {
		t.Fatalf("stage1Calls after first event = %d, want 1", stage1Calls)
	}

	for i := 0; i < 3; i++ {
		if err := flowdb.RecordAttentionFeedback(db, flowdb.AttentionFeedback{
			ID: "cascade-learn-" + string(rune('a'+i)), FeedItemID: "feed", Source: "slack",
			Channel: "C_NOISE", Author: "U_OTHER", ThreadType: "channel", ThreadKey: "C_NOISE:1.1",
			SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed",
			Confidence: 0.82, ConfidenceBand: "0.80-0.89", CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("record feedback %d: %v", i, err)
		}
	}

	if err := c.Observe(context.Background(), msg("C_NOISE", "2.1", "U_OTHER", "second")); err != nil {
		t.Fatalf("second Observe: %v", err)
	}
	if stage1Calls != 1 {
		t.Errorf("stage1Calls after learned suppression = %d, want still 1 (Stage 0 drop)", stage1Calls)
	}
}
