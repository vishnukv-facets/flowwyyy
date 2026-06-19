package flowbackup

import (
	"os"
	"path/filepath"
)

// gitignoreContent is a secondary safety net. The authoritative control over
// what gets committed is the explicit per-file staging in checkpointLocked
// (only curated markdown under the curated trees is ever `Add`ed). This ignore
// list additionally guarantees that large, secret, or high-churn paths are
// never staged — protecting against an accidental manual git operation and
// keeping go-git's own status walks cheap. The flow root holds a ~476MB
// flow.db, a .ui-session-token secret, logs, caches, and agent session JSONL;
// none of these should ever enter the backup repo.
const gitignoreContent = `# flow backup repository — managed by flow. Do not edit.
#
# What IS versioned: curated markdown under kb/, projects/, tasks/, playbooks/,
# owners/ (brief.md + updates/*.md + kb/*.md). The exact set is chosen in code;
# this list is a secondary guard so the items below are never committed.

# Database (huge, binary, high-churn) — backed up separately as rotated snapshots.
flow.db
flow.db-*
*.sqlite
*.sqlite-*

# Secrets and runtime/config state.
.ui-session-token
*.json
*.jsonl
*.log
*.cursor
tmux.conf
.DS_Store

# Foreign / generated trees.
.backupgit/
.claude/
.codex/
cache/
logs/
backups/
**/workspace/
**/.git/
.flow-backup.lock
`

// writeGitignore writes (or refreshes) the whitelist .gitignore at the repo
// root. Idempotent — always rewrites to the canonical content so an upgraded
// flow can tighten the rules on existing repos.
func writeGitignore(root string) error {
	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == gitignoreContent {
		return nil
	}
	return os.WriteFile(path, []byte(gitignoreContent), 0o644)
}
