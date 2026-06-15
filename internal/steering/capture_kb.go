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

// captureKBRunner runs a hidden `claude -p` session that distills an
// operator-approved event into a durable KB fact and appends it to the right
// kb/*.md file. Unlike send-reply (which needs a connector MCP to post), this is
// pure local filesystem work — reading + editing markdown under the flow KB dir
// — which a headless bypass session does natively, no MCP required. Mockable in
// tests.
var captureKBRunner = func(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p", prompt,
		"--model", CaptureKBModel(), "--dangerously-skip-permissions", "--output-format", "json")
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), commandError("steering: capture-kb claude -p", err, out)
	}
	return decodeClaudeJSONOutput(ctx, "capture_kb", out)
}

// CaptureKBModel is the model the ephemeral KB-capture session runs under.
// Capture is judgment-heavy file editing (pick the right file, dedup, phrase a
// durable fact), which Haiku does unreliably; Sonnet handles it cleanly at far
// less cost than the operator's Opus default. Override via
// FLOW_STEERING_CAPTURE_MODEL.
func CaptureKBModel() string {
	if m := strings.TrimSpace(os.Getenv("FLOW_STEERING_CAPTURE_MODEL")); m != "" {
		return m
	}
	return "claude-sonnet-4-6"
}

// CaptureKBViaAgent records an operator-approved event as durable knowledge in
// the KB (kb/*.md under kbDir), WITHOUT spawning a visible flow task. A hidden
// bypass agent distills the event into one concise fact, picks the best KB file
// (or confirms it's already covered), appends it with a dated provenance line,
// and reports CAPTURED. On a confirmed capture the feed item is marked acted; on
// anything else the card stays 'new' so the operator sees it wasn't captured and
// can retry — never a silent false "captured". Mirrors SendReplyViaAgent.
func CaptureKBViaAgent(ctx context.Context, db *sql.DB, item flowdb.FeedItem, kbDir string) error {
	if strings.TrimSpace(kbDir) == "" {
		return fmt.Errorf("steering: capture-kb requires a kb directory")
	}
	out, err := captureKBRunner(withSteeringUsage(ctx, db, "capture_kb"), captureKBPrompt(item, kbDir))
	trimmed := strings.TrimSpace(out)
	// Always log what the agent actually did — the only window into a headless run.
	fmt.Fprintf(os.Stderr, "steering: capture-kb agent for %s replied: %s\n", item.ID, truncate(trimmed, 600))
	if err != nil {
		return err
	}
	if !captureConfirmed(trimmed) {
		return fmt.Errorf("steering: capture-kb not confirmed (agent said: %s)", truncate(trimmed, 200))
	}
	if err := flowdb.SetFeedItemActed(db, item.ID, "", nowRFC3339()); err != nil {
		return err
	}
	return recordActionFeedback(db, item, string(ActionCaptureKB), "captured", "")
}

// captureConfirmed reports whether the agent explicitly confirmed a KB write. We
// require the convention token "CAPTURED" and reject anything that also signals
// failure, so a chatty "I couldn't write…" never counts as success.
func captureConfirmed(out string) bool {
	up := strings.ToUpper(out)
	if up == "" || strings.HasPrefix(up, "FAILED") || strings.HasPrefix(up, "ERROR") {
		return false
	}
	if strings.Contains(up, "CANNOT") || strings.Contains(up, "UNABLE") || strings.Contains(up, "DON'T HAVE") || strings.Contains(up, "NOT AVAILABLE") {
		return false
	}
	return strings.Contains(up, "CAPTURED")
}

func captureKBPrompt(item flowdb.FeedItem, kbDir string) string {
	kbDir = strings.TrimRight(strings.TrimSpace(kbDir), "/")
	summary := strings.TrimSpace(item.Summary)
	if summary == "" {
		summary = strings.TrimSpace(item.Reason)
	}
	return `MODE: capture-kb

You are the KB-capture step of an operator's attention router. The operator
approved saving the durable knowledge in the message below into their knowledge
base. Capture it now — do not ask for confirmation.

The KB lives in this directory (one durable-facts markdown file per scope):
  ` + kbDir + `/user.md       — facts about the operator personally
  ` + kbDir + `/org.md        — people, teams, orgs, who-owns-what
  ` + kbDir + `/products.md   — products, services, systems, architecture
  ` + kbDir + `/processes.md  — how things are done; conventions; workflows
  ` + kbDir + `/business.md   — business context, customers, priorities

Steps:
1. READ the relevant existing KB file(s) first.
2. Distill the event into ONE concise, durable fact (1–3 lines) — a decision, a
   plan, or an org/process/product fact. Strip transient phrasing; write it as a
   standing truth, not "someone said today". If it's a PLAN or intention not yet
   carried out, mark it provisional with an "(plan, as of ` + nowRFC3339()[:10] + `)" suffix so a
   later close-out sweep can recognize and settle it once the work is done.
3. Pick the single best-fit file and APPEND the fact, followed by a short dated
   provenance line, e.g. "(source: ` + sourceLabel(item) + `, captured ` + nowRFC3339()[:10] + `)".
4. DEDUP: if the same fact is already present, update/refine it in place instead
   of duplicating. If it's already fully covered, that still counts as captured.
5. Refer to people and channels by name; never paste raw platform IDs.

Source: ` + item.Source + ` thread ` + item.ThreadKey + `

Knowledge to capture:
` + summary + `

When you have written (or confirmed) the fact, reply with a single line:
CAPTURED <relative kb file path>
If you could not write for ANY reason (no file access, the dir is missing, etc.),
reply with a single line: FAILED: <short reason>. Do NOT pretend you captured it.
Output nothing else.`
}
