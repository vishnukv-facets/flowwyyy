package steering

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// PendingRemovalHeading is the exact per-file section heading the dreaming pass
// moves stale entries under, and that the deterministic prune scans. Kept as a
// shared constant so the agent prompt, the prune, and any UI agree on it.
const PendingRemovalHeading = "## ⚠️ Pending removal"

// DreamKBViaAgent runs the KB hygiene ("dreaming") pass: a headless agent reads
// the KB files, finds entries that have gone STALE / SUPERSEDED / CONTRADICTED /
// INCORRECT, and MOVES each (never deletes) into a per-file "Pending removal"
// section, annotated with a machine-readable [flagged YYYY-MM-DD] marker and a
// one-line reason. The operator then curates; a deterministic prune removes
// entries left flagged for too long. Reviews the KB FILES (not a conversation),
// so a headless session over the files is the right tool — same runner as
// capture-kb. Returns the agent's reply (logged by the caller).
func DreamKBViaAgent(ctx context.Context, kbDir string) (string, error) {
	if strings.TrimSpace(kbDir) == "" {
		return "", fmt.Errorf("steering: kb-dream requires a kb directory")
	}
	out, err := captureKBRunner(ctx, dreamKBPrompt(kbDir))
	trimmed := strings.TrimSpace(out)
	fmt.Fprintf(os.Stderr, "steering: kb-dream agent replied: %s\n", truncate(trimmed, 600))
	if err != nil {
		return trimmed, err
	}
	return trimmed, nil
}

func dreamKBPrompt(kbDir string) string {
	kbDir = strings.TrimRight(strings.TrimSpace(kbDir), "/")
	today := nowRFC3339()[:10]
	return `MODE: kb-dream (hygiene)

You are the knowledge-base hygiene pass ("dreaming"). Your job is to keep the KB
CORRECT and CURRENT — it loads into every future task brief, so a stale or wrong
fact misleads every future session.

The KB lives in this directory (one durable-facts markdown file per scope):
  ` + kbDir + `/user.md
  ` + kbDir + `/org.md
  ` + kbDir + `/products.md
  ` + kbDir + `/processes.md
  ` + kbDir + `/business.md

Steps:
1. READ each KB file.
2. Within each file, identify entries that are now one of:
   - STALE — a plan/intention later carried out, or a fact no longer true.
   - SUPERSEDED — a newer entry in the same file replaces or corrects it.
   - CONTRADICTED — another entry or later reality contradicts it.
   - INCORRECT — demonstrably wrong.
   Be CONSERVATIVE. When unsure whether something is still true, LEAVE IT. Do not
   flag entries that are merely old but still accurate. Most files need no changes.
3. For each entry you do flag, MOVE it (cut it from where it is) into a section
   in the SAME file titled EXACTLY:
   ` + PendingRemovalHeading + `
   Create that section at the END of the file if it doesn't exist. Rewrite the
   moved entry on one line as:
   - [flagged ` + today + `] <the original entry text, trimmed to one line> — why: <one short reason it is stale>
4. NEVER delete an entry outright — only MOVE it into Pending removal. The
   operator reviews that section; entries left there too long are pruned
   automatically. Do NOT touch entries already under a Pending removal heading
   (they're already flagged).
5. Do not rewrite or reword entries you are keeping.

When done, reply with a single line: DREAMED <number of entries flagged> (0 is a
fine, common answer). Write the files silently; output nothing else.`
}
