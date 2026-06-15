package steering

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"flow/internal/flowdb"
)

// sendReplyRunner runs a hidden Haiku `claude -p` session that POSTS an
// operator-approved reply via the agent's own connector MCP (Slack/GitHub) — the
// same headless, bypass-permission agent layer the triage stages already use to
// READ threads. Bypass is correct here: the operator approved the exact text via
// the attention feed, so there's nothing left to gate. Mockable in tests.
var sendReplyRunner = func(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt,
		"--model", classifierModel(), "--dangerously-skip-permissions", "--output-format", "json")
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), commandError("steering: send-reply claude -p", err, out)
	}
	return decodeClaudeJSONOutput(ctx, "send_reply", out)
}

// SendReplyViaAgent posts an operator-approved reply WITHOUT spawning a visible
// flow task. A hidden Haiku agent session (bypass + MCP) posts the reply to the
// source thread, then the feed item is marked acted. This is the operator's
// intended flow: the hidden triage-layer session that surfaced the item sends
// it, rather than creating a new task that then trips the auto-mode permission
// gate. Returns an error (leaving the card unresolved so it can be retried) when
// the agent reports it could not post.
func SendReplyViaAgent(ctx context.Context, db *sql.DB, item flowdb.FeedItem, text, instructions string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("steering: send-reply requires non-empty text")
	}
	out, err := sendReplyRunner(withSteeringUsage(ctx, db, "send_reply"), sendReplyPrompt(item, text, instructions))
	trimmed := strings.TrimSpace(out)
	// Always log what the agent actually said — the only way to see whether it
	// posted, refused, or lacked the connector MCP in a headless run.
	fmt.Fprintf(os.Stderr, "steering: send-reply agent for %s replied: %s\n", item.ID, truncate(trimmed, 600))
	if err != nil {
		return err
	}
	// STRICT success: only mark acted when the agent explicitly confirms it posted.
	// Anything else (a refusal, an explanation that it has no Slack/GitHub MCP
	// tool, an empty reply) leaves the card 'new' so it's visibly unsent and can
	// be retried — never a silent false "acted".
	if !postConfirmed(trimmed) {
		return fmt.Errorf("steering: send-reply not confirmed posted (agent said: %s)", truncate(trimmed, 200))
	}
	// No linked task — the hidden agent posted directly.
	if err := flowdb.SetFeedItemActed(db, item.ID, "", nowRFC3339()); err != nil {
		return err
	}
	return recordActionFeedback(db, item, "send_reply", "approved", text)
}

// postConfirmed reports whether the agent's reply is an explicit posting
// confirmation. We require the convention token "POSTED" and reject anything
// that also signals failure, so a chatty "I couldn't post…" never counts.
func postConfirmed(out string) bool {
	up := strings.ToUpper(out)
	if up == "" || strings.HasPrefix(up, "FAILED") || strings.HasPrefix(up, "ERROR") {
		return false
	}
	if strings.Contains(up, "CANNOT") || strings.Contains(up, "UNABLE") || strings.Contains(up, "DON'T HAVE") || strings.Contains(up, "NO SLACK") || strings.Contains(up, "NOT AVAILABLE") {
		return false
	}
	return strings.Contains(up, "POSTED")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sendReplyPrompt(item flowdb.FeedItem, text, instructions string) string {
	// With no extra instructions: post the approved draft as-is (minor wording
	// only). With instructions: the draft is a starting point — revise it per the
	// operator's instructions, then post.
	draftClause := "The operator has ALREADY APPROVED the reply below and asked you to POST it NOW. Do not ask for confirmation and do not redraft beyond minor wording — just post it."
	if ins := strings.TrimSpace(instructions); ins != "" {
		draftClause = "The operator approved sending a reply on this thread and gave you specific instructions. Start from the draft below, APPLY the operator's instructions to revise it, then POST the result. Do not ask for confirmation.\n\nOperator instructions:\n" + ins
	}
	return `MODE: send-reply

You are the send step of an operator's attention router. ` + draftClause + `

1. Post the reply to the source thread. ` + sendHintFor(item.Source) + `
2. Refer to people and channels by name; never paste raw platform IDs.

Source: ` + item.Source + ` thread ` + item.ThreadKey + `

Draft reply:
` + strings.TrimSpace(text) + `

When you have actually posted, reply with a single line: POSTED
If you could not post for ANY reason (the required tool isn't available, an API
error, etc.), reply with a single line: FAILED: <short reason>. Do NOT pretend
you posted. Output nothing else.`
}

// SendReplyModel is the model the ephemeral Slack send session runs under.
// Posting needs a tool-capable model: Haiku has the Slack MCP tool but reliably
// fumbles the call (tries Bash/echo, then gives up "can't invoke the tool"),
// whereas Sonnet/Opus invoke it cleanly. So this defaults to Sonnet — far cheaper
// than the user's Opus default but reliable for the single MCP call — decoupled
// from the classifier model (text classification is fine on Haiku; driving a tool
// is not). Override via FLOW_STEERING_SEND_MODEL (e.g. to force opus, or to retry
// haiku once tool-calling improves).
func SendReplyModel() string {
	if m := strings.TrimSpace(os.Getenv("FLOW_STEERING_SEND_MODEL")); m != "" {
		return m
	}
	return "claude-sonnet-4-6"
}

// SlackSendSessionPrompt is the brief for the ephemeral, watchable floating send
// session (see Server.prepareSendReplyFloatingLaunch). Unlike SendReplyViaAgent
// — which uses a headless `claude -p` that has NO claude.ai connector MCPs and so
// cannot post to Slack — this runs in a real interactive Claude session that DOES
// have the Slack MCP. It posts the operator-approved reply and, on a confirmed
// post ONLY, runs doneCmd (which marks the feed card sent and closes the window).
// On any failure it stops and leaves the window open so the operator sees why.
func SlackSendSessionPrompt(item flowdb.FeedItem, text, instructions, doneCmd string) string {
	draftClause := "The operator has ALREADY APPROVED the reply below and asked you to POST it NOW. Do not ask for confirmation and do not redraft beyond minor wording — just post it."
	if ins := strings.TrimSpace(instructions); ins != "" {
		draftClause = "The operator approved sending a reply on this thread and gave you specific instructions. Start from the draft below, APPLY the operator's instructions to revise it, then POST the result. Do not ask for confirmation.\n\nOperator instructions:\n" + ins
	}
	channelLine := ""
	if ch := strings.TrimSpace(item.Channel); ch != "" {
		channelLine = "\nChannel: " + ch
	}
	return `MODE: send-reply (ephemeral session)

You are a short-lived send agent for the operator's attention router. ` + draftClause + `

1. Post the reply by CALLING the Slack MCP tool DIRECTLY — make a tool call to ` + "`mcp__claude_ai_Slack__slack_send_message`" + ` with the channel, the thread_ts (so it's threaded in-thread), and the message text. This is a TOOL CALL, not a shell command: do NOT use Bash, echo, cat, printf, or any shell to post — the shell cannot post to Slack, and "printing" or echoing the message does NOT send it. The thread_key is "<channel>:<thread_ts>" (channel id before the ":", thread_ts after).
2. Refer to people and channels by name in the message body; never paste raw platform IDs.

Source: ` + item.Source + ` thread ` + item.ThreadKey + channelLine + `

Draft reply:
` + strings.TrimSpace(text) + `

When — and ONLY when — the Slack tool call has RETURNED SUCCESSFULLY, run this one shell command (the ONLY thing you use the shell for) to mark the card sent and close this window:

    ` + doneCmd + `

If you CANNOT post for ANY reason (the Slack tool errors or isn't available, a missing channel, etc.), do NOT run that command. Instead print one short line explaining what failed, then stop — leave this window open so the operator can see the problem. Do not fall back to Bash/echo to "send" — that is not a send.`
}

// sendHintFor gives the connector-specific POSTING instruction. Slack posts go
// through the Slack MCP; GitHub posts go through the `gh` CLI (always available
// via Bash under bypass — no GitHub MCP needed).
func sendHintFor(source string) string {
	switch source {
	case "github":
		return "Use the `gh` CLI (NOT a GitHub MCP). The thread_key encodes owner/repo plus gh-pr/gh-issue#<number>; post a comment with `gh pr comment <number> --repo <owner/repo> --body \"…\"` (or `gh issue comment` for an issue)."
	default:
		return "Use the Slack MCP tools (e.g. mcp__claude_ai_Slack__slack_send_message) to post threaded in-thread on the thread_ts. The thread_key is \"<channel>:<thread_ts>\"."
	}
}
