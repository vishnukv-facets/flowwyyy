package steering

import (
	"flow/internal/productdb"
	"testing"
)

func TestForwardSourceTrusted(t *testing.T) {
	cases := []struct {
		name            string
		item            productdb.FeedItem
		slackSelf       string
		ghSelf          string
		trustedChannels string
		want            bool
	}{
		{
			name:      "slack self-authored is trusted",
			item:      productdb.FeedItem{Source: "slack", Author: "U_SELF"},
			slackSelf: "U_SELF,U_OTHER",
			want:      true,
		},
		{
			name:   "github self-login is trusted",
			item:   productdb.FeedItem{Source: "github", Author: "octo-me"},
			ghSelf: "octo-me",
			want:   true,
		},
		{
			name:            "allowlisted channel is trusted even from a stranger",
			item:            productdb.FeedItem{Source: "slack", Author: "U_STRANGER", Channel: "C_TEAM"},
			trustedChannels: "C_TEAM C_OPS",
			want:            true,
		},
		{
			name:      "stranger on an unlisted channel is untrusted",
			item:      productdb.FeedItem{Source: "slack", Author: "U_STRANGER", Channel: "C_RANDOM"},
			slackSelf: "U_SELF",
			want:      false,
		},
		{
			name: "empty author and channel with no config is untrusted (fail closed)",
			item: productdb.FeedItem{Source: "slack"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLOW_SLACK_SELF_USER_IDS", tc.slackSelf)
			t.Setenv("FLOW_GH_SELF_LOGINS", tc.ghSelf)
			t.Setenv("FLOW_STEERING_TRUSTED_CHANNELS", tc.trustedChannels)
			if got := ForwardSourceTrusted(tc.item); got != tc.want {
				t.Fatalf("ForwardSourceTrusted(%+v) = %v, want %v", tc.item, got, tc.want)
			}
		})
	}
}
