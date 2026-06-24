package steering

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"flow/internal/productdb"
)

// sendReplyRunner runs a hidden Haiku `claude -p` session that POSTS an
// operator-approved reply via the agent's own connector MCP (Slack/GitHub) — the
// same headless, bypass-permission agent layer the triage stages already use to
// READ threads. Bypass is correct here: the operator approved the exact text via
// the attention feed, so there's nothing left to gate. Mockable in tests.
var sendReplyRunner = func(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt,
		"--model", classifierModel(), "--dangerously-skip-permissions")
	out, err := cmd.CombinedOutput() // capture stderr too — surfaces MCP/tool failures
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("steering: send-reply claude -p: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// SendReplyViaAgent posts an operator-approved reply WITHOUT spawning a visible
// flow task. A hidden Haiku agent session (bypass + MCP) posts the reply to the
// source thread, then the feed item is marked acted. This is the operator's
// intended flow: the hidden triage-layer session that surfaced the item sends
// it, rather than creating a new task that then trips the auto-mode permission
// gate. Returns an error (leaving the card unresolved so it can be retried) when
// the agent reports it could not post.
func SendReplyViaAgent(ctx context.Context, db *sql.DB, item productdb.FeedItem, text, instructions string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("steering: send-reply requires non-empty text")
	}
	out, err := sendReplyRunner(ctx, sendReplyPrompt(item, text, instructions))
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
	if err := productdb.SetFeedItemActed(db, item.ID, "", nowRFC3339()); err != nil {
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

func sendReplyPrompt(item productdb.FeedItem, text, instructions string) string {
	// With no extra instructions: post the approved draft as-is (minor wording
	// only). With instructions: the draft is a starting point — revise it per the
	// operator's instructions, then post.
	draftClause := "The operator has ALREADY APPROVED the reply below and asked you to POST it NOW. Do not ask for confirmation and do not redraft beyond minor wording — just post it."
	if ins := strings.TrimSpace(instructions); ins != "" {
		draftClause = "The operator approved sending a reply on this thread and gave you specific instructions. Start from the draft below, APPLY the operator's instructions to revise it, then POST the result. Do not ask for confirmation.\n\nOperator instructions:\n" + ins
	}
	return `MODE: send-reply

You are the send step of an operator's attention router. ` + draftClause + operatorVoiceDirective() + `

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
