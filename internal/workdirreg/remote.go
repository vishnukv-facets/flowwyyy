package workdirreg

import (
	"database/sql"
	"flow/internal/flowdb"
	"flow/internal/gitremote"
	"fmt"
)

// DetectGitRemote is re-exported from internal/gitremote so existing callers of
// workdirreg.DetectGitRemote keep working. The detection itself is flowdb-free
// and lives in gitremote (T13 split); the DB-backed registry below stays here.
var DetectGitRemote = gitremote.DetectGitRemote

// Register records a workdir and captures its current origin remote when one
// exists. Existing names/descriptions are preserved unless non-empty values are
// supplied by the caller.
func Register(db *sql.DB, path, name, description string) error {
	return flowdb.UpsertWorkdir(db, path, name, description, DetectGitRemote(path))
}

// Touch records recent use for a workdir and refreshes git_remote if the path
// currently exposes an origin remote.
func Touch(db *sql.DB, path string) error {
	now := flowdb.NowISO()
	remote := DetectGitRemote(path)
	var (
		res sql.Result
		err error
	)
	if remote != "" {
		res, err = db.Exec(`UPDATE workdirs SET last_used_at = ?, git_remote = ? WHERE path = ?`, now, remote, path)
	} else {
		res, err = db.Exec(`UPDATE workdirs SET last_used_at = ? WHERE path = ?`, now, path)
	}
	if err != nil {
		return err
	}
	if changed, err := res.RowsAffected(); err == nil && changed == 0 {
		return flowdb.UpsertWorkdir(db, path, "", "", remote)
	}
	return nil
}

// SyncGitRemotes is a one-shot backfill/update for registered workdirs. It
// never clears stored remotes; it only writes a remote that is currently
// discoverable from the local git checkout.
func SyncGitRemotes(db *sql.DB) (int, error) {
	workdirs, err := flowdb.ListWorkdirs(db)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, w := range workdirs {
		remote := DetectGitRemote(w.Path)
		if remote == "" {
			continue
		}
		if w.GitRemote.Valid && w.GitRemote.String == remote {
			continue
		}
		if _, err := db.Exec(`UPDATE workdirs SET git_remote = ? WHERE path = ?`, remote, w.Path); err != nil {
			return updated, fmt.Errorf("sync workdir remote %s: %w", w.Path, err)
		}
		updated++
	}
	return updated, nil
}
