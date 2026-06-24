package steering

import (
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func TestWatchConfigFromEnv(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me, U_alt")
	t.Setenv("FLOW_STEERING_WATCH_CHANNELS", "C1 C2,C3")
	t.Setenv("FLOW_STEERING_MUTED_CHANNELS", "C_mute")
	t.Setenv("FLOW_STEERING_MUTED_KEYWORDS", "lunch, standup")

	cfg := WatchConfigFromEnv()

	for _, c := range []string{"C1", "C2", "C3"} {
		if !cfg.WatchedChannels[c] {
			t.Errorf("watched channel %s missing: %v", c, cfg.WatchedChannels)
		}
	}
	if !cfg.MutedChannels["C_mute"] {
		t.Errorf("muted channels = %v", cfg.MutedChannels)
	}
	if len(cfg.MutedKeywords) != 2 || cfg.MutedKeywords[0] != "lunch" {
		t.Errorf("muted keywords = %v", cfg.MutedKeywords)
	}
	if len(cfg.Identity.UserIDs) != 2 || len(cfg.MentionUserIDs) != 2 {
		t.Errorf("identity/mention should both come from SelfUserIDs: %v / %v", cfg.Identity.UserIDs, cfg.MentionUserIDs)
	}
}

func TestWatchConfigFromEnvEmpty(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "")
	t.Setenv("FLOW_SLACK_SELF_USER_ID", "")
	t.Setenv("FLOW_SLACK_USER_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	t.Setenv("FLOW_STEERING_WATCH_CHANNELS", "")
	cfg := WatchConfigFromEnv()
	if len(cfg.WatchedChannels) != 0 || len(cfg.Identity.UserIDs) != 0 {
		t.Errorf("empty env should yield empty config, got %+v", cfg)
	}
}

func TestWatchConfigFnWithMutesOverlaysLearnedSuppressions(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		if err := productdb.RecordAttentionFeedback(db, productdb.AttentionFeedback{
			ID: "learned-config-" + string(rune('a'+i)), FeedItemID: "feed", Source: "slack",
			Channel: "C_NOISE", Author: "U_NOISE", ThreadType: "channel", ThreadKey: "C_NOISE:1",
			SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed",
			Confidence: 0.85, ConfidenceBand: "0.80-0.89", CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("record feedback %d: %v", i, err)
		}
	}

	cfg := WatchConfigFnWithMutes(db)()
	if !cfg.MutedThreads["C_NOISE:1"] {
		t.Errorf("learned dismissed thread not overlaid into MutedThreads: %+v", cfg.MutedThreads)
	}
	// Learned suppression must NOT mute the whole channel — only the thread.
	if cfg.MutedChannels["C_NOISE"] {
		t.Errorf("learned suppression must not mute the channel, only the thread: %+v", cfg.MutedChannels)
	}
	if !cfg.MutedAuthors["U_NOISE"] {
		t.Errorf("learned dismissed author not overlaid into MutedAuthors: %+v", cfg.MutedAuthors)
	}
}

func TestWatchConfigFnWithMutesLearnsThreadNotChannel(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// Repeated dismissals in ONE thread of a watched channel.
	for i := 0; i < 3; i++ {
		if err := productdb.RecordAttentionFeedback(db, productdb.AttentionFeedback{
			ID: "thread-learn-" + string(rune('a'+i)), FeedItemID: "feed", Source: "slack",
			Channel: "C_WATCHED", Author: "U_X", ThreadType: "channel", ThreadKey: "C_WATCHED:111.1",
			SuggestedAction: "reply", FinalAction: "dismiss", Outcome: "dismissed",
			Confidence: 0.85, ConfidenceBand: "0.80-0.89", CreatedAt: "2026-06-05T10:00:00Z",
		}); err != nil {
			t.Fatalf("record feedback %d: %v", i, err)
		}
	}

	t.Setenv("FLOW_STEERING_WATCH_CHANNELS", "C_WATCHED")
	cfg := WatchConfigFnWithMutes(db)()
	// The dismissed THREAD is muted...
	if !cfg.MutedThreads["C_WATCHED:111.1"] {
		t.Errorf("repeatedly dismissed thread should be learned-suppressed: %+v", cfg.MutedThreads)
	}
	// ...but the channel as a whole is NOT — other threads in it still surface.
	if cfg.MutedChannels["C_WATCHED"] {
		t.Errorf("learned suppression must never mute the whole channel: %+v", cfg.MutedChannels)
	}

	// An explicit channel mute still wins.
	if err := productdb.AddSteeringMute(db, productdb.MuteScopeChannel, "C_WATCHED"); err != nil {
		t.Fatalf("AddSteeringMute: %v", err)
	}
	cfg = WatchConfigFnWithMutes(db)()
	if !cfg.MutedChannels["C_WATCHED"] {
		t.Errorf("explicit channel mute should still apply: %+v", cfg.MutedChannels)
	}
}

func TestWatchConfigFnWithMutesIncludesTaskLinkedGitHubThreads(t *testing.T) {
	db, err := flowdb.OpenDB(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	now := "2026-06-07T08:00:00Z"
	if _, err := db.Exec(`INSERT INTO tasks (slug,name,status,priority,work_dir,session_provider,created_at,updated_at)
		VALUES ('autonomy-trust-ladder','Autonomy trust ladder','in-progress','high',?,'codex',?,?)`, t.TempDir(), now, now); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "autonomy-trust-ladder", "gh-pr:vishnukv-facets/flow-manager#21"); err != nil {
		t.Fatalf("AddTaskTag: %v", err)
	}

	cfg := WatchConfigFnWithMutes(db)()
	key := "vishnukv-facets/flow-manager:gh-pr:vishnukv-facets/flow-manager#21"
	if !cfg.TaskLinkedGitHubThreads[key] {
		t.Fatalf("task-linked github key %q missing from %+v", key, cfg.TaskLinkedGitHubThreads)
	}
}
