package steering

import (
	"encoding/json"
	"testing"
)

func TestParseAction(t *testing.T) {
	cases := []struct {
		in     string
		want   Action
		wantOK bool
	}{
		{"make_task", ActionMakeTask, true},
		{"forward", ActionForward, true},
		{"reply", ActionReply, true},
		{"afk_reply", ActionAFKReply, true},
		{"digest_only", ActionDigestOnly, true},
		{"drop", ActionDrop, true},
		{"MAKE_TASK", ActionMakeTask, true},
		{"  forward  ", ActionForward, true},
		{"nonsense", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := ParseAction(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("ParseAction(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func TestVerdictJSONRoundTrip(t *testing.T) {
	in := Verdict{
		Source:            "slack",
		ThreadKey:         "C123:1700000000.000100",
		SuggestedAction:   ActionMakeTask,
		MatchedTask:       "kong-split",
		SuggestedProject:  "goniyo",
		SuggestedPriority: "high",
		Urgency:           UrgencyUrgent,
		IsVIP:             true,
		Confidence:        0.91,
		Summary:           "Customer asks for rollout date",
		Draft:             "On it — targeting Friday.",
		Reason:            "names operator + question mark",
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Verdict
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}
