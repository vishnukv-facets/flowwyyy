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
		"--model", CaptureKBModel(), "--dangerously-skip-permissions")
	out, err := cmd.CombinedOutput() // capture stderr too — surfaces tool/file failures
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("steering: capture-kb claude -p: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
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
	if err := captureKBEffect(ctx, db, item, kbDir); err != nil {
		return err
	}
	return recordActionFeedback(db, item, string(ActionCaptureKB), "captured", "")
}

// captureKBEffect runs the hidden capture agent and, on a confirmed write, marks
// the card acted — WITHOUT recording an operator-feedback row. It is the shared
// core behind the operator path (CaptureKBViaAgent, which adds the feedback row)
// and the autonomous path (ApplyActionAuto, which skips it so an auto-capture
// can't inflate the calibrator it gated on). On anything but a confirmed capture
// it returns an error and leaves the card 'new', so the operator sees it wasn't
// captured and can retry — never a silent false "captured".
func captureKBEffect(ctx context.Context, db *sql.DB, item flowdb.FeedItem, kbDir string) error {
	if strings.TrimSpace(kbDir) == "" {
		return fmt.Errorf("steering: capture-kb requires a kb directory")
	}
	out, err := captureKBRunner(ctx, captureKBPrompt(item, kbDir))
	trimmed := strings.TrimSpace(out)
	// Always log what the agent actually did — the only window into a headless run.
	fmt.Fprintf(os.Stderr, "steering: capture-kb agent for %s replied: %s\n", item.ID, truncate(trimmed, 600))
	if err != nil {
		return err
	}
	if !captureConfirmed(trimmed) {
		return fmt.Errorf("steering: capture-kb not confirmed (agent said: %s)", truncate(trimmed, 200))
	}
	return flowdb.SetFeedItemActed(db, item.ID, "", nowRFC3339())
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

// PromoteCorrectionToKB distills an operator correction into a durable KB fact
// (kb/*.md) — the "remember this generally" option on the correction button. It
// reuses the operator-reply KB path (operator's own words → KB): NOTHING-DURABLE
// is a clean no-op, so a thread-specific correction that isn't a general fact
// simply isn't written. Best-effort — the caller logs and proceeds on error.
func PromoteCorrectionToKB(ctx context.Context, threadKey, source, text, kbDir string) error {
	return captureOperatorReplyKB(ctx, threadKey, source, text, kbDir)
}

// captureOperatorReplyKB distills a durable fact out of a reply the operator
// wrote BY HAND on a watched, already-triaged thread and appends it to the KB —
// the steerer "learns the operator's voice/decisions" path. Unlike
// CaptureKBViaAgent (operator clicked "capture" on a card), there's no feed item
// and no certainty the reply carries a durable fact, so the prompt is allowed to
// answer NOTHING-DURABLE and we treat that as a clean no-op. Best-effort: returns
// an error only on a real failure (the caller logs and moves on); NOTHING-DURABLE
// and CAPTURED both succeed.
func captureOperatorReplyKB(ctx context.Context, threadKey, source, text, kbDir string) error {
	if strings.TrimSpace(kbDir) == "" {
		return fmt.Errorf("steering: capture operator-reply kb requires a kb directory")
	}
	out, err := captureKBRunner(ctx, operatorReplyKBPrompt(threadKey, source, text, kbDir))
	trimmed := strings.TrimSpace(out)
	fmt.Fprintf(os.Stderr, "steering: operator-reply kb agent for %s replied: %s\n", threadKey, truncate(trimmed, 600))
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(trimmed), "NOTHING-DURABLE") {
		return nil // operator's reply carried nothing worth keeping — expected, not a failure
	}
	if !captureConfirmed(trimmed) {
		return fmt.Errorf("steering: operator-reply kb not confirmed (agent said: %s)", truncate(trimmed, 200))
	}
	return nil
}

func operatorReplyKBPrompt(threadKey, source, text, kbDir string) string {
	kbDir = strings.TrimRight(strings.TrimSpace(kbDir), "/")
	return `MODE: capture-kb (operator hand-written reply)

You are the learning step of an operator's attention router. The operator replied
BY HAND on a watched conversation. Their own words are the strongest signal of how
they think, decide, and phrase things. Capture ONLY a durable fact if the reply
carries one — do not invent or stretch.

The KB lives in this directory (one durable-facts markdown file per scope):
  ` + kbDir + `/user.md       — facts about the operator personally
  ` + kbDir + `/org.md        — people, teams, orgs, who-owns-what
  ` + kbDir + `/products.md   — products, services, systems, architecture
  ` + kbDir + `/processes.md  — how things are done; conventions; workflows
  ` + kbDir + `/business.md   — business context, customers, priorities

Steps:
1. Decide whether the reply contains a DURABLE fact — a decision, a standing
   preference, an org/process/product/business truth. Transient chatter
   ("ok", "thanks", "looking now", "will check") is NOT durable.
2. If NOT durable, reply with the single line: NOTHING-DURABLE — and stop.
3. If durable: READ the best-fit KB file, distill ONE concise fact (1–3 lines),
   write it as a standing truth (strip "I just said today" phrasing). If it's a
   PLAN not yet carried out, suffix "(plan, as of ` + nowRFC3339()[:10] + `)".
4. APPEND it with a dated provenance line, e.g.
   "(source: ` + source + ` thread ` + threadKey + `, captured ` + nowRFC3339()[:10] + `)".
5. DEDUP: if already present, refine in place instead of duplicating. Already
   covered still counts as captured.
6. Refer to people and channels by name; never paste raw platform IDs.

Source: ` + source + ` thread ` + threadKey + `

The operator's hand-written reply:
` + strings.TrimSpace(text) + `

Reply with ONE line only:
  CAPTURED <relative kb file path>   (you wrote/confirmed a durable fact)
  NOTHING-DURABLE                    (no durable fact in the reply)
  FAILED: <short reason>             (could not write for any reason)
Output nothing else.`
}

// substantive is a cheap pre-filter so the operator-reply KB agent is not spawned
// for trivial acknowledgements ("ok", "thanks", "👍", "on it"). The agent makes the
// real durability call; this only avoids an LLM round-trip on obvious noise.
// ponytail: word-count heuristic (a 1-token emoji/ack is <4 fields); tighten only
// if noise slips through.
func substantive(text string) bool {
	return len(strings.Fields(text)) >= 4
}
