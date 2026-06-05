package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// countingNameClient is a SlackTitleClient that records how many times each
// id was looked up, so cache hits/misses can be asserted.
type countingNameClient struct {
	users     map[string]SlackUser
	chans     map[string]SlackConversation
	userCalls map[string]int
	chanCalls map[string]int
	err       error
}

func newCountingNameClient() *countingNameClient {
	return &countingNameClient{
		users:     map[string]SlackUser{},
		chans:     map[string]SlackConversation{},
		userCalls: map[string]int{},
		chanCalls: map[string]int{},
	}
}

func (c *countingNameClient) ConversationInfo(_ context.Context, channelID string) (SlackConversation, error) {
	c.chanCalls[channelID]++
	if c.err != nil {
		return SlackConversation{}, c.err
	}
	conv, ok := c.chans[channelID]
	if !ok {
		return SlackConversation{}, errors.New("not found")
	}
	return conv, nil
}

func (c *countingNameClient) ConversationReplies(_ context.Context, _, _ string, _ int) ([]SlackMessage, error) {
	return nil, nil
}

func (c *countingNameClient) UsersInConversation(_ context.Context, _ string, _ int) ([]string, error) {
	return nil, nil
}

func (c *countingNameClient) UserInfo(_ context.Context, userID string) (SlackUser, error) {
	c.userCalls[userID]++
	if c.err != nil {
		return SlackUser{}, c.err
	}
	u, ok := c.users[userID]
	if !ok {
		return SlackUser{}, errors.New("not found")
	}
	return u, nil
}

func TestSlackNameResolverWarmDedupesAndCaches(t *testing.T) {
	client := newCountingNameClient()
	client.users["U1"] = SlackUser{ID: "U1", DisplayName: "Vishnu"}
	client.chans["C1"] = SlackConversation{ID: "C1", Name: "general", IsChannel: true}
	r := NewSlackNameResolverWithClient(client)

	// Duplicate IDs in the warm set must each resolve exactly once.
	r.Warm(context.Background(), []string{"U1", "U1", ""}, []string{"C1", "C1", ""})

	if client.userCalls["U1"] != 1 {
		t.Fatalf("U1 user calls = %d, want 1 (deduped)", client.userCalls["U1"])
	}
	if client.chanCalls["C1"] != 1 {
		t.Fatalf("C1 chan calls = %d, want 1 (deduped)", client.chanCalls["C1"])
	}
	// After warming, name lookups are cache hits — no further API calls.
	if got := r.UserName(context.Background(), "U1"); got != "Vishnu" {
		t.Fatalf("UserName = %q, want Vishnu", got)
	}
	if got := r.ChannelName(context.Background(), "C1"); got != "#general" {
		t.Fatalf("ChannelName = %q, want #general", got)
	}
	if client.userCalls["U1"] != 1 || client.chanCalls["C1"] != 1 {
		t.Fatalf("post-warm lookups hit the API: userCalls=%d chanCalls=%d, want 1/1",
			client.userCalls["U1"], client.chanCalls["C1"])
	}
	// Warming an already-cached set is a no-op (no extra calls).
	r.Warm(context.Background(), []string{"U1"}, []string{"C1"})
	if client.userCalls["U1"] != 1 || client.chanCalls["C1"] != 1 {
		t.Fatalf("re-warm hit the API: userCalls=%d chanCalls=%d, want 1/1",
			client.userCalls["U1"], client.chanCalls["C1"])
	}
}

// TestSlackNameResolverWarmNil ensures Warm is nil-safe (no token → nil resolver).
func TestSlackNameResolverWarmNil(t *testing.T) {
	var r *SlackNameResolver
	r.Warm(context.Background(), []string{"U1"}, []string{"C1"}) // must not panic
}

func TestSlackNameResolverUserNameResolvesAndCaches(t *testing.T) {
	client := newCountingNameClient()
	client.users["U1"] = SlackUser{ID: "U1", DisplayName: "Vishnu", RealName: "Vishnu KV"}
	r := NewSlackNameResolverWithClient(client)

	if got := r.UserName(context.Background(), "U1"); got != "Vishnu" {
		t.Fatalf("UserName = %q, want Vishnu", got)
	}
	if got := r.UserName(context.Background(), "U1"); got != "Vishnu" {
		t.Fatalf("UserName (cached) = %q, want Vishnu", got)
	}
	if client.userCalls["U1"] != 1 {
		t.Fatalf("UserInfo called %d times, want 1 (second should hit cache)", client.userCalls["U1"])
	}
}

func TestSlackNameResolverUserNameFallsBackThroughFields(t *testing.T) {
	client := newCountingNameClient()
	client.users["U_real"] = SlackUser{ID: "U_real", RealName: "Real Name"}
	client.users["U_name"] = SlackUser{ID: "U_name", Name: "handle"}
	r := NewSlackNameResolverWithClient(client)

	if got := r.UserName(context.Background(), "U_real"); got != "Real Name" {
		t.Fatalf("UserName = %q, want Real Name", got)
	}
	if got := r.UserName(context.Background(), "U_name"); got != "handle" {
		t.Fatalf("UserName = %q, want handle", got)
	}
}

func TestSlackNameResolverNegativeLookupCachedNoRawID(t *testing.T) {
	client := newCountingNameClient() // no users registered → every lookup errors
	r := NewSlackNameResolverWithClient(client)

	if got := r.UserName(context.Background(), "U_missing"); got != "" {
		t.Fatalf("UserName = %q, want empty (never the raw id)", got)
	}
	_ = r.UserName(context.Background(), "U_missing")
	if client.userCalls["U_missing"] != 1 {
		t.Fatalf("UserInfo called %d times, want 1 (negative result should cache)", client.userCalls["U_missing"])
	}
}

func TestSlackNameResolverChannelNamePrefixesHash(t *testing.T) {
	client := newCountingNameClient()
	client.chans["C1"] = SlackConversation{ID: "C1", Name: "coinswitch", IsChannel: true}
	client.chans["C2"] = SlackConversation{ID: "C2", Name: "#already", IsChannel: true}
	r := NewSlackNameResolverWithClient(client)

	if got := r.ChannelName(context.Background(), "C1"); got != "#coinswitch" {
		t.Fatalf("ChannelName = %q, want #coinswitch", got)
	}
	if got := r.ChannelName(context.Background(), "C2"); got != "#already" {
		t.Fatalf("ChannelName = %q, want #already (no double prefix)", got)
	}
}

func TestSlackNameResolverCleanTextResolvesMentionsAndLinks(t *testing.T) {
	client := newCountingNameClient()
	client.users["U1"] = SlackUser{ID: "U1", DisplayName: "Vishnu"}
	r := NewSlackNameResolverWithClient(client)

	cases := []struct {
		in   string
		want string
	}{
		{"hey <@U1> look", "hey @Vishnu look"},
		{"cc <@U1|vishnu.kv>", "cc @vishnu.kv"},                 // inline label preferred
		{"see <https://example.com|the docs>", "see the docs"}, // labelled link
		{"raw <https://example.com>", "raw https://example.com"},
		{"unknown <@U0MISSING> here", "unknown @user here"}, // unresolved → @user, never the id
		{"line one\nline two", "line one\nline two"},         // newlines preserved
	}
	for _, tc := range cases {
		if got := r.CleanText(context.Background(), tc.in); got != tc.want {
			t.Errorf("CleanText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlackNameResolverNilIsSafe(t *testing.T) {
	var r *SlackNameResolver // simulates "no token configured"
	if got := r.UserName(context.Background(), "U1"); got != "" {
		t.Fatalf("nil UserName = %q, want empty", got)
	}
	if got := r.ChannelName(context.Background(), "C1"); got != "" {
		t.Fatalf("nil ChannelName = %q, want empty", got)
	}
	// CleanText must still strip markup and never leak an id even with no resolver.
	if got := r.CleanText(context.Background(), "hi <@U1> and <https://x.io|x>"); got != "hi @user and x" {
		t.Fatalf("nil CleanText = %q, want %q", got, "hi @user and x")
	}
}

func TestSlackNameResolverTTLExpiry(t *testing.T) {
	client := newCountingNameClient()
	client.users["U1"] = SlackUser{ID: "U1", DisplayName: "Vishnu"}
	r := NewSlackNameResolverWithClient(client)
	r.ttl = time.Millisecond

	_ = r.UserName(context.Background(), "U1")
	time.Sleep(5 * time.Millisecond)
	_ = r.UserName(context.Background(), "U1")
	if client.userCalls["U1"] != 2 {
		t.Fatalf("UserInfo called %d times, want 2 (cache should expire past TTL)", client.userCalls["U1"])
	}
}

func TestMentionedUserIDs(t *testing.T) {
	got := MentionedUserIDs("hi <@U123> and <@U456|alice>, ping <@U123> again")
	want := []string{"U123", "U456"} // distinct, order-preserving
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
	if MentionedUserIDs("no mentions here") != nil {
		t.Error("text with no mentions should return nil")
	}
}
