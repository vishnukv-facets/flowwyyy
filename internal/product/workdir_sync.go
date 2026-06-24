package product

import (
	"database/sql"
	"fmt"
	"os/exec"
	"strings"

	"flow/internal/gitremote"
	"flow/internal/productdb"
)

// syncWorkdirGitRemotes refreshes the git_remote of every registered workdir
// whose detected remote changed — the flowdb-free, product-side twin of
// workdirreg.SyncGitRemotes (Phase-3 decoupling, seam §11). It reads the
// registry via productdb and detects remotes via the flowdb-free gitremote
// package, but workdirs is Bucket O, so the update is routed through
// `flow workdir add <path>` exec (which re-detects + upserts git_remote) rather
// than a direct DB write. Best-effort and bounded by the workdir count; returns
// how many were refreshed. flowBin is the resolved core `flow` binary.
func syncWorkdirGitRemotes(db *sql.DB, flowBin string) (int, error) {
	wds, err := productdb.ListWorkdirs(db)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, w := range wds {
		remote := gitremote.DetectGitRemote(w.Path)
		if remote == "" {
			continue
		}
		if w.GitRemote.Valid && w.GitRemote.String == remote {
			continue
		}
		// `flow workdir add` re-detects the remote and upserts it (COALESCE keeps
		// the existing name/description). The CLI is the documented Bucket-O write
		// surface; we don't poke the workdirs table directly.
		out, err := exec.Command(flowBin, "workdir", "add", w.Path).CombinedOutput()
		if err != nil {
			return updated, fmt.Errorf("sync workdir remote %s: %w (%s)", w.Path, err, strings.TrimSpace(string(out)))
		}
		updated++
	}
	return updated, nil
}
