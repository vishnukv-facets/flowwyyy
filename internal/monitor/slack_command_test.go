package monitor

import "testing"

func TestIsCommandChannel_MatchesBotIMOnly(t *testing.T) {
	orig := conversationIsBotIMFn
	// Bot is a member of D_bot only; D_colleague is a third-party DM (bot absent).
	conversationIsBotIMFn = func(channel string) bool { return channel == "D_bot" }
	defer func() { conversationIsBotIMFn = orig }()
	resetCommandChannelCache()

	yes := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_me"}
	if !IsCommandChannel(yes) {
		t.Errorf("DM where the bot is a member should be a command channel")
	}
	no := InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_colleague", UserID: "U_me"}
	if IsCommandChannel(no) {
		t.Errorf("operator's DM with a third party (bot not a member) must NOT be the command channel")
	}
	chMsg := InboundEvent{Kind: "message", ChannelType: "channel", Channel: "D_bot"}
	if IsCommandChannel(chMsg) {
		t.Errorf("non-im event must not match")
	}
	resetCommandChannelCache()
}

// TestResetCommandChannelCache_ForcesReResolution verifies that the bot-IM
// membership cache memoizes per-channel and that ResetCommandChannelCache()
// forces conversationIsBotIMFn to be invoked again instead of returning the
// cached result.
func TestResetCommandChannelCache_ForcesReResolution(t *testing.T) {
	orig := conversationIsBotIMFn
	defer func() {
		conversationIsBotIMFn = orig
		resetCommandChannelCache()
	}()
	resetCommandChannelCache()

	callCount := 0
	conversationIsBotIMFn = func(channel string) bool {
		callCount++
		return true
	}

	// First lookup resolves (counter = 1).
	if !botIsMemberOfIM("D_test") {
		t.Fatalf("expected D_test to resolve as a bot IM")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 resolve call after first botIsMemberOfIM(), got %d", callCount)
	}

	// Second lookup hits the cache — counter stays at 1.
	_ = botIsMemberOfIM("D_test")
	if callCount != 1 {
		t.Fatalf("expected cache hit on second botIsMemberOfIM(), resolver call count should still be 1, got %d", callCount)
	}

	// ResetCommandChannelCache clears the cache; next lookup must re-resolve.
	ResetCommandChannelCache()
	_ = botIsMemberOfIM("D_test")
	if callCount != 2 {
		t.Fatalf("expected 2 resolve calls after ResetCommandChannelCache()+botIsMemberOfIM(), got %d", callCount)
	}
}

func TestCommandChannelEnabled(t *testing.T) {
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "")
	if CommandChannelEnabled() {
		t.Errorf("default should be disabled")
	}
	t.Setenv("FLOW_SLACK_COMMAND_ENABLED", "1")
	if !CommandChannelEnabled() {
		t.Errorf("=1 should enable")
	}
}

func TestAuthorizedOperator(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me, U_alt") // space between IDs tests whitespace tolerance
	if !AuthorizedOperator("U_me") || !AuthorizedOperator("U_alt") {
		t.Errorf("listed operator IDs must be authorized")
	}
	if AuthorizedOperator("U_other") {
		t.Errorf("non-operator must NOT be authorized")
	}
	if AuthorizedOperator("") {
		t.Errorf("empty author must NOT be authorized")
	}
}

// TestAuthorizedOperator_TokenOwnerFallback verifies the robustness fix: the
// operator's own id may not appear in FLOW_SLACK_SELF_USER_IDS (Enterprise-Grid
// alternate id / stale env), but AuthorizedOperator still accepts them when their
// id matches the USER-token owner resolved via auth.test.
func TestAuthorizedOperator_TokenOwnerFallback(t *testing.T) {
	orig := operatorUserIDFn
	defer func() {
		operatorUserIDFn = orig
		resetCommandChannelCache()
	}()
	operatorUserIDFn = func() string { return "U_token_owner" }

	cases := []struct {
		name    string
		selfIDs string
		userID  string
		want    bool
	}{
		{"in self IDs", "U_me", "U_me", true},
		{"not in self IDs but is token owner", "U_me", "U_token_owner", true},
		{"neither self ID nor token owner", "U_me", "U_other", false},
		{"empty author", "U_me", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetCommandChannelCache() // reset memoized token owner between cases
			t.Setenv("FLOW_SLACK_SELF_USER_IDS", tc.selfIDs)
			if got := AuthorizedOperator(tc.userID); got != tc.want {
				t.Errorf("AuthorizedOperator(%q) with selfIDs=%q = %v, want %v", tc.userID, tc.selfIDs, got, tc.want)
			}
		})
	}
}

// TestOperatorIdentityKnown verifies that identity is "known" when EITHER the
// self-IDs env is set OR the token owner resolves, and unknown only when both
// are empty.
func TestOperatorIdentityKnown(t *testing.T) {
	orig := operatorUserIDFn
	defer func() {
		operatorUserIDFn = orig
		resetCommandChannelCache()
	}()

	// Self IDs set, token unresolved → known.
	operatorUserIDFn = func() string { return "" }
	resetCommandChannelCache()
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "U_me")
	if !OperatorIdentityKnown() {
		t.Errorf("identity should be known when self IDs are set")
	}

	// Self IDs empty, token owner resolves → known.
	operatorUserIDFn = func() string { return "U_token_owner" }
	resetCommandChannelCache()
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "")
	if !OperatorIdentityKnown() {
		t.Errorf("identity should be known when only the token owner resolves")
	}

	// Both empty → unknown.
	operatorUserIDFn = func() string { return "" }
	resetCommandChannelCache()
	t.Setenv("FLOW_SLACK_SELF_USER_IDS", "")
	if OperatorIdentityKnown() {
		t.Errorf("identity should be unknown when both self IDs and token owner are empty")
	}
}

// TestIsSelfAuthoredSlack verifies flow recognizes its OWN bot's messages
// (echoed back through the listener) by the resolved bot user id, and never
// mistakes the operator or third parties for itself. Fail-safe: an unresolved
// bot id (empty) matches nothing, so real traffic is processed, not swallowed.
func TestIsSelfAuthoredSlack(t *testing.T) {
	orig := selfBotUserIDFn
	defer func() {
		selfBotUserIDFn = orig
		resetCommandChannelCache()
	}()

	// Bot id resolves to U_bot.
	selfBotUserIDFn = func() string { return "U_bot" }
	resetCommandChannelCache()
	cases := []struct {
		name string
		ev   InboundEvent
		want bool
	}{
		{"flow's own bot message", InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_bot"}, true},
		{"operator message", InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: "U_me"}, false},
		{"third party", InboundEvent{Kind: "message", ChannelType: "channel", Channel: "C1", UserID: "U_other"}, false},
		{"empty author", InboundEvent{Kind: "message", ChannelType: "im", Channel: "D_bot", UserID: ""}, false},
		{"github login that isn't us", InboundEvent{Kind: "message", ChannelType: "github", UserID: "octocat"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSelfAuthoredSlack(tc.ev); got != tc.want {
				t.Errorf("IsSelfAuthoredSlack(%+v) = %v, want %v", tc.ev, got, tc.want)
			}
		})
	}

	// Fail-safe: when the bot id can't be resolved, nothing is self-authored.
	selfBotUserIDFn = func() string { return "" }
	resetCommandChannelCache()
	if IsSelfAuthoredSlack(InboundEvent{Kind: "message", ChannelType: "im", UserID: "U_bot"}) {
		t.Errorf("unresolved bot id must make IsSelfAuthoredSlack false (fail-safe)")
	}
}

// TestSelfBotUserIDs verifies the operator-configured bot-id set parses like
// the operator self-id list (comma/space split, trim, dedupe) and is nil when
// unset.
func TestSelfBotUserIDs(t *testing.T) {
	t.Setenv("FLOW_SLACK_SELF_BOT_USER_IDS", " U_bot1 , U_bot2 U_bot1 ")
	got := SelfBotUserIDs()
	if len(got) != 2 || got[0] != "U_bot1" || got[1] != "U_bot2" {
		t.Errorf("SelfBotUserIDs() = %v, want [U_bot1 U_bot2] (trimmed, split, deduped)", got)
	}

	t.Setenv("FLOW_SLACK_SELF_BOT_USER_IDS", "")
	t.Setenv("FLOW_SLACK_SELF_BOT_USER_ID", "U_solo")
	if got := SelfBotUserIDs(); len(got) != 1 || got[0] != "U_solo" {
		t.Errorf("SelfBotUserIDs() singular fallback = %v, want [U_solo]", got)
	}

	t.Setenv("FLOW_SLACK_SELF_BOT_USER_IDS", "")
	t.Setenv("FLOW_SLACK_SELF_BOT_USER_ID", "")
	if got := SelfBotUserIDs(); got != nil {
		t.Errorf("SelfBotUserIDs() with unset env = %v, want nil", got)
	}
}

// TestSlackBotOnlyToken verifies that the bot-id resolver token source accepts
// ONLY genuine bot tokens (xoxb-) and never the operator's user token (xoxp-).
// This is the fix for the user-token-only bug: SlackBotToken() falls back to the
// user token, so auth.test on it returns the OPERATOR, poisoning self-echo
// detection. Filtering by the xoxb- prefix keeps the bot-id resolution honest.
func TestSlackBotOnlyToken(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"dedicated bot token", map[string]string{"SLACK_BOT_TOKEN": "xoxb-bot"}, "xoxb-bot"},
		{"flow token holding a bot token", map[string]string{"FLOW_SLACK_TOKEN": "xoxb-flowbot"}, "xoxb-flowbot"},
		{"write token holding a bot token", map[string]string{"FLOW_SLACK_WRITE_TOKEN": "xoxb-write"}, "xoxb-write"},
		{"write token wins (flow posts with it)", map[string]string{"FLOW_SLACK_WRITE_TOKEN": "xoxb-write", "SLACK_BOT_TOKEN": "xoxb-bot"}, "xoxb-write"},
		{"user token only is ignored", map[string]string{"SLACK_USER_TOKEN": "xoxp-user"}, ""},
		{"user token in the bot slot is ignored", map[string]string{"FLOW_SLACK_TOKEN": "xoxp-user"}, ""},
		{"user write token is ignored", map[string]string{"SLACK_WRITE_TOKEN": "xoxp-user"}, ""},
		{"nothing configured", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"FLOW_SLACK_WRITE_TOKEN", "SLACK_WRITE_TOKEN", "SLACK_BOT_TOKEN", "FLOW_SLACK_TOKEN", "SLACK_TOKEN", "SLACK_USER_TOKEN"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := slackBotOnlyToken(); got != tc.want {
				t.Errorf("slackBotOnlyToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsSelfAuthoredSlack_ConfiguredBotID is the regression test for the
// user-token-only deployment bug. When auth.test can't resolve the bot id (it
// returns "" because the only token is the operator's user token), the operator
// can pin FLOW_SLACK_SELF_BOT_USER_IDS so flow still recognizes and drops its own
// bot's echoes — without mistaking the OPERATOR for itself (which would drop the
// operator's own command DMs).
func TestIsSelfAuthoredSlack_ConfiguredBotID(t *testing.T) {
	orig := selfBotUserIDFn
	defer func() {
		selfBotUserIDFn = orig
		resetCommandChannelCache()
	}()
	// auth.test resolution unavailable/wrong (user-token-only) → "".
	selfBotUserIDFn = func() string { return "" }
	resetCommandChannelCache()
	t.Setenv("FLOW_SLACK_SELF_BOT_USER_IDS", "U0BA6B7DQKV, U_bot2")

	if !IsSelfAuthoredSlack(InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", UserID: "U0BA6B7DQKV"}) {
		t.Errorf("configured self-bot id must be recognized as self even when auth.test is unresolved")
	}
	if !IsSelfAuthoredSlack(InboundEvent{Kind: "message", Channel: "C1", UserID: "U_bot2"}) {
		t.Errorf("every configured self-bot id must be recognized as self")
	}
	// The operator (NOT in the bot set) must never be treated as self — otherwise
	// the dispatcher's self-echo drop would swallow the operator's own command DMs.
	if IsSelfAuthoredSlack(InboundEvent{Kind: "message", ChannelType: "im", Channel: "D1", UserID: "U03HNAFLVAN"}) {
		t.Errorf("operator id must NOT be self-authored (would break the command channel)")
	}
}

// TestResetCommandChannelCache_ClearsSelfBotID verifies the bot user id is
// memoized once and that ResetCommandChannelCache() forces re-resolution
// (the bot token changes on reinstall).
func TestResetCommandChannelCache_ClearsSelfBotID(t *testing.T) {
	orig := selfBotUserIDFn
	defer func() {
		selfBotUserIDFn = orig
		resetCommandChannelCache()
	}()
	resetCommandChannelCache()

	callCount := 0
	selfBotUserIDFn = func() string {
		callCount++
		return "U_bot"
	}

	if selfBotUserID() != "U_bot" {
		t.Fatalf("expected bot id to resolve")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 resolve call, got %d", callCount)
	}
	_ = selfBotUserID()
	if callCount != 1 {
		t.Fatalf("expected cache hit (count still 1), got %d", callCount)
	}
	ResetCommandChannelCache()
	_ = selfBotUserID()
	if callCount != 2 {
		t.Fatalf("expected 2 resolve calls after reset, got %d", callCount)
	}
}

// TestResetCommandChannelCache_ClearsOperatorID verifies the token-owner id is
// memoized once and that ResetCommandChannelCache() forces re-resolution.
func TestResetCommandChannelCache_ClearsOperatorID(t *testing.T) {
	orig := operatorUserIDFn
	defer func() {
		operatorUserIDFn = orig
		resetCommandChannelCache()
	}()
	resetCommandChannelCache()

	callCount := 0
	operatorUserIDFn = func() string {
		callCount++
		return "U_token_owner"
	}

	if operatorUserID() != "U_token_owner" {
		t.Fatalf("expected token owner to resolve")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 resolve call, got %d", callCount)
	}
	// Cached — no re-resolution.
	_ = operatorUserID()
	if callCount != 1 {
		t.Fatalf("expected cache hit (count still 1), got %d", callCount)
	}
	// Reset forces re-resolution.
	ResetCommandChannelCache()
	_ = operatorUserID()
	if callCount != 2 {
		t.Fatalf("expected 2 resolve calls after reset, got %d", callCount)
	}
}
