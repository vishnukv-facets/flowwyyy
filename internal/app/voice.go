package app

import (
	"fmt"

	"flow/internal/steering"
)

// cmdVoice prints the operator's configured Slack/GitHub reply voice — the
// "Voice" saved in Mission Control → Attention → config, persisted to
// persona.md. It returns the SAME text that the steerer's per-channel sessions
// and the stateless triage/send steps inject, so a task-agent session following
// the skill can fetch it and draft an outbound reply in the operator's voice
// instead of a generic assistant tone. Falls back to the built-in default when
// nothing is saved, so it always prints a usable voice.
func cmdVoice(args []string) int {
	fs := flagSet("voice")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Println(steering.OperatorVoice())
	return 0
}
