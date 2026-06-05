// internal/steering/triage.go
package steering

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// deepTriageRunner shells out to the capable (default) Claude model for the
// Stage 3 deep triage: it reads full context (e.g. via the Slack MCP), drafts
// a reply, and emits a final Verdict. Tests swap this var. Unlike the cheap
// classifier it does NOT pin --model, so the operator's default (capable)
// model is used.
var deepTriageRunner = func(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt, "--dangerously-skip-permissions")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("steering: deep triage claude -p: %w", err)
	}
	return string(out), nil
}

// DeepTriage runs Stage 3 on a single survivor: a capable headless agent that
// gathers full context and emits the final Verdict (including a drafted reply
// when appropriate). The draft is SURFACED only — P1 never auto-sends it.
func DeepTriage(ctx context.Context, in ClassifyInput, taskIndex string) (Verdict, error) {
	raw, err := deepTriageRunner(ctx, deepTriagePrompt(in, taskIndex))
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(raw, in.Source, in.ThreadKey)
}

// contextHintFor returns the connector-specific instruction for how the deep
// triage step should read the full surrounding context. Adding a connector
// means adding a case here (plus its connectorOf discriminator and Stage 0
// policy). Unknown sources fall back to the Slack hint (default connector).
func contextHintFor(source string) string {
	switch source {
	case "github":
		return "For GitHub, use the `gh` CLI or the GitHub MCP to read the full PR/issue and its comments. The thread_key encodes owner/repo plus gh-pr/gh-issue#<number>; the item URL is the canonical link."
	default:
		return "For Slack, use the Slack MCP tools to read the thread (channel + thread_ts are encoded in thread_key as \"<channel>:<thread_ts>\")."
	}
}

func deepTriagePrompt(in ClassifyInput, taskIndex string) string {
	payload, _ := json.Marshal(in)
	return `MODE: stage3-deep

You are the deep-triage step of an operator's attention router. A cheap gate has already decided this message is worth a closer look. Do the following, then emit a single verdict:

1. Read the full surrounding context. ` + contextHintFor(in.Source) + `
2. Decide whether this message belongs to an EXISTING task (set matched_task) or warrants a new one. Do NOT decide from the task name alone — for any plausibly related task (especially ones in the project this message seems to belong to), use your file tools to READ that task's brief.md AND the progress notes in its updates/ directory (paths are given in the index below) before judging. A message belongs to an existing task when it continues, follows up on, or is the next step of the work that task covers — even if it arrives in a different Slack thread/DM. Prefer matched_task to an existing active task in such cases; only treat it as net-new when, after reading, no active task actually covers it.
3. If a reply from the operator is appropriate, draft it in the operator's voice. DO NOT SEND ANYTHING — the draft is surfaced for the operator's approval only.

Always refer to people and channels by name; never output raw platform IDs (e.g. Slack user IDs like U0123, channel IDs like C0123).

Respond with ONLY a minified JSON object (no prose, fences allowed but optional):
{"suggested_action":"make_task|forward|reply|afk_reply|digest_only|drop","matched_task":"<slug or empty>","suggested_project":"<slug or empty>","suggested_priority":"high|medium|low","urgency":"urgent|normal|low","confidence":0.0,"summary":"<= 140 chars","draft":"<reply text, if any>","reason":"<why>"}

Operator task/project index:
` + taskIndex + `

Message (JSON):
` + string(payload)
}
