package monitor

import (
	"os"
	"testing"
)

// IsExternalToOrg is the gate's keystone: a conversation is "outside the org"
// when it includes people from another organization. Two independent signals —
// Slack Connect flags (which hold even when we don't know our own team) and a
// participating team_id that isn't ours (only usable when our team is known).
func TestSlackConversationIsExternalToOrg(t *testing.T) {
	const ownTeam = "T_OWN"
	owned := []string{ownTeam}
	cases := []struct {
		name    string
		conv    SlackConversation
		teams   []string
		wantExt bool
	}{
		{"internal channel", SlackConversation{ID: "C1", ContextTeamID: ownTeam}, owned, false},
		{"ext_shared (connect)", SlackConversation{ID: "C1", IsExtShared: true}, owned, true},
		{"org_shared", SlackConversation{ID: "C1", IsOrgShared: true}, owned, true},
		{"pending_ext_shared", SlackConversation{ID: "C1", IsPendingExtShared: true}, owned, true},
		{"connect flag without known own team", SlackConversation{ID: "C1", IsExtShared: true}, nil, true},
		{"foreign connected team", SlackConversation{ID: "C1", ConnectedTeamIDs: []string{"T_OTHER"}}, owned, true},
		{"foreign shared team", SlackConversation{ID: "C1", SharedTeamIDs: []string{ownTeam, "T_OTHER"}}, owned, true},
		{"foreign context team", SlackConversation{ID: "C1", ContextTeamID: "T_OTHER"}, owned, true},
		{"only our team present", SlackConversation{ID: "C1", ConnectedTeamIDs: []string{ownTeam}, ContextTeamID: ownTeam}, owned, false},
		// Team mismatch is only decidable when we know our own team(s): with no
		// configured team, a foreign team_id alone must NOT gate (avoid gating
		// every channel before the operator's team is seeded).
		{"foreign team but own team unknown", SlackConversation{ID: "C1", ConnectedTeamIDs: []string{"T_OTHER"}}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.conv.IsExternalToOrg(tc.teams); got != tc.wantExt {
				t.Fatalf("IsExternalToOrg(%v) = %v, want %v", tc.teams, got, tc.wantExt)
			}
		})
	}
}

func TestOperatorTeamIDs(t *testing.T) {
	for _, k := range []string{"FLOW_SLACK_TEAM_IDS", "FLOW_SLACK_TEAM_ID"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if got := OperatorTeamIDs(); len(got) != 0 {
		t.Fatalf("no env set: OperatorTeamIDs() = %v, want empty", got)
	}
	t.Setenv("FLOW_SLACK_TEAM_IDS", "T1, T2 T1")
	got := OperatorTeamIDs()
	if len(got) != 2 || got[0] != "T1" || got[1] != "T2" {
		t.Fatalf("OperatorTeamIDs() = %v, want [T1 T2] de-duped", got)
	}
}
