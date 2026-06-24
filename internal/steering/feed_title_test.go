package steering

import (
	"flow/internal/productdb"
	"strings"
	"testing"
)

func TestTitleFromSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "clause break on em-dash",
			in:   "CoinSwitch thread: asks to rename the CloudSQL DB to an options-ex name — IaC rename within the active csx migration.",
			want: "CoinSwitch thread: asks to rename the CloudSQL DB to an options-ex name",
		},
		{
			name: "short summary kept whole",
			in:   "PR #71 review pass, suite green",
			want: "PR #71 review pass, suite green",
		},
		{
			name: "no break, word-boundary truncate (never mid-word)",
			in:   "Follow up asking Vishnu whether the reports were generated successfully and shared with the team yet today",
			// must not end mid-word and must carry the ellipsis
			want: "",
		},
	}
	for _, c := range cases {
		got := titleFromSummary(c.in, feedTaskNameMaxLen)
		if c.want != "" && got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if len([]rune(got)) > feedTaskNameMaxLen+1 { // +1 for the ellipsis rune
			t.Errorf("%s: title too long (%d runes): %q", c.name, len([]rune(got)), got)
		}
		// A truncated title must not split the last word: it ends on a full word
		// (optionally followed by the ellipsis), never a partial token.
		if strings.HasSuffix(got, "…") {
			body := strings.TrimSuffix(got, "…")
			if strings.HasSuffix(body, " ") || body == "" {
				t.Errorf("%s: bad truncation %q", c.name, got)
			}
		}
	}
}

func TestFeedTaskNameFallback(t *testing.T) {
	// Empty summary falls back to the thread key, not a blank title.
	got := feedTaskName(productdb.FeedItem{ThreadKey: "slack-c1:1.2"})
	if got != "Attention: slack-c1:1.2" {
		t.Fatalf("empty-summary fallback: got %q", got)
	}
}
