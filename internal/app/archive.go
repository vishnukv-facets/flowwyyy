package app

import (
	"database/sql"
	"flow/internal/flowdb"
	"fmt"
	"os"
)

// cmdArchive sets archived_at = now() on the matching row.
func cmdArchive(args []string) int {
	return setArchivedAt(args, true)
}

// cmdUnarchive clears archived_at.
func cmdUnarchive(args []string) int {
	return setArchivedAt(args, false)
}

// setArchivedAt is the shared mutation: archive=true sets archived_at to
// now, archive=false clears it.
func setArchivedAt(args []string, archive bool) int {
	verb := "archive"
	pastVerb := "Archived"
	if !archive {
		verb = "unarchive"
		pastVerb = "Unarchived"
	}
	fs := flagSet(verb)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "error: %s requires exactly one ref\n", verb)
		return 2
	}
	ref := fs.Arg(0)

	dbPath, err := flowDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	// For unarchive we must include archived rows; for archive we exclude them.
	includeArchived := !archive
	kind, slug, err := ResolveTaskProjectOrPlaybook(db, ref, includeArchived)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	now := flowdb.NowISO()
	// Archiving a project or playbook cascades to the tasks it owns (and
	// unarchiving restores them), so a container and its work move together.
	var table, childCol string
	switch kind {
	case "task":
		table = "tasks"
	case "project":
		table = "projects"
		childCol = "project_slug"
	case "playbook":
		table = "playbooks"
		childCol = "playbook_slug"
	}

	tx, err := db.Begin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer tx.Rollback()

	if archive {
		_, err = tx.Exec(fmt.Sprintf("UPDATE %s SET archived_at = ?, updated_at = ? WHERE slug = ?", table), now, now, slug)
	} else {
		_, err = tx.Exec(fmt.Sprintf("UPDATE %s SET archived_at = NULL, updated_at = ? WHERE slug = ?", table), now, slug)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cascaded := 0
	if childCol != "" {
		var res sql.Result
		if archive {
			res, err = tx.Exec(
				fmt.Sprintf("UPDATE tasks SET archived_at = ?, updated_at = ? WHERE %s = ? AND archived_at IS NULL AND deleted_at IS NULL", childCol),
				now, now, slug,
			)
		} else {
			res, err = tx.Exec(
				fmt.Sprintf("UPDATE tasks SET archived_at = NULL, updated_at = ? WHERE %s = ? AND archived_at IS NOT NULL AND deleted_at IS NULL", childCol),
				now, slug,
			)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			cascaded = int(n)
		}
	}

	if err := tx.Commit(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("%s %s %s", pastVerb, kind, slug)
	if cascaded > 0 {
		noun := "task"
		if cascaded != 1 {
			noun = "tasks"
		}
		verbed := "archived"
		if !archive {
			verbed = "unarchived"
		}
		fmt.Printf(" (%d %s also %s)", cascaded, noun, verbed)
	}
	fmt.Println()
	return 0
}
