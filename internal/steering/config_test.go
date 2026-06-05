package steering

import "testing"

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
