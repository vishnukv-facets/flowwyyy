package app

import (
	"bytes"
	"flow/internal/flowbackup"
	"fmt"
	"os"
	"path/filepath"
)

// cmdBackup is the operator-facing entry point to the ~/.flow backup safety net:
// inspect the checkpoint history and roll a curated markdown file (a kb file or
// any brief/update) back to a previous version without scraping transcripts.
//
//	flow backup status
//	flow backup list [<relpath>]
//	flow backup show <rev> <relpath>
//	flow backup diff <rev> <relpath>
//	flow backup restore <relpath> [--at <rev>]
//	flow backup now
func cmdBackup(args []string) int {
	if len(args) == 0 {
		return backupUsage()
	}
	sub, rest := args[0], args[1:]
	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	switch sub {
	case "status":
		return backupStatus(root)
	case "list", "log":
		return backupList(root, rest)
	case "show":
		return backupShow(root, rest)
	case "diff":
		return backupDiff(root, rest)
	case "restore":
		return backupRestore(root, rest)
	case "now":
		return backupNow(root)
	case "remote":
		return backupRemote(root, rest)
	case "push":
		return backupPush(root)
	case "-h", "--help", "help":
		return backupUsage()
	}
	fmt.Fprintf(os.Stderr, "error: unknown backup subcommand %q\n", sub)
	return backupUsage()
}

func backupUsage() int {
	fmt.Println(`flow backup — durable version history for ~/.flow curated markdown

  flow backup status                      repo state, last checkpoint, db snapshots
  flow backup list [<relpath>]            list checkpoints (optionally for one file)
  flow backup show <rev> <relpath>        print a file's content at a revision
  flow backup diff <rev> <relpath>        diff a revision against the current file
  flow backup restore <relpath> [--at <rev>]   roll a file back (defaults to previous version)
  flow backup now                         force a checkpoint + db snapshot now
  flow backup remote github               provision a PRIVATE GitHub repo (via gh) + use it
  flow backup remote set <url>            configure a PRIVATE offsite git remote
  flow backup remote show                 print the configured remote
  flow backup remote clear                remove the offsite remote
  flow backup push                        push markdown + latest db snapshot offsite

<relpath> is relative to the flow root, e.g. kb/org.md or tasks/<slug>/brief.md`)
	return 0
}

func backupRemote(root string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: remote requires set|show|clear")
		return 2
	}
	switch args[0] {
	case "show":
		if url := flowbackup.RemoteURL(root); url != "" {
			fmt.Println(url)
		} else {
			fmt.Println("(no offsite remote configured)")
		}
		return 0
	case "set":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "error: remote set requires a <url> (use a PRIVATE repo — your KB leaves this machine)")
			return 2
		}
		if err := flowbackup.SetRemote(root, args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("Offsite remote set to %s\n", args[1])
		fmt.Println("Note: use a PRIVATE repository — your KB contains personal/org facts.")
		return 0
	case "clear":
		if err := flowbackup.ClearRemote(root); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println("Offsite remote removed.")
		return 0
	case "github":
		if !flowbackup.GitHubBackupAvailable() {
			fmt.Fprintln(os.Stderr, "error: GitHub CLI (gh) is not authenticated. Run `gh auth login` (needs the 'repo' scope) and retry.")
			return 1
		}
		url, created, err := flowbackup.EnsureGitHubRemote(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		if created {
			fmt.Printf("Created private GitHub backup repo and set it as the offsite remote:\n  %s\n", url)
		} else {
			fmt.Printf("Using existing private GitHub backup repo as the offsite remote:\n  %s\n", url)
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "error: unknown remote subcommand %q\n", args[0])
	return 2
}

func backupPush(root string) int {
	if !flowbackup.RemoteConfigured(root) {
		fmt.Fprintln(os.Stderr, "error: no offsite remote configured — set one with: flow backup remote set <url>")
		return 1
	}
	if err := flowbackup.Push(root); err != nil {
		fmt.Fprintf(os.Stderr, "error: push markdown: %v\n", err)
		return 1
	}
	if err := flowbackup.PushDBSnapshot(root, flowbackup.LatestDBSnapshot(root)); err != nil {
		fmt.Fprintf(os.Stderr, "error: push db snapshot: %v\n", err)
		return 1
	}
	fmt.Println("Pushed markdown + latest db snapshot to the offsite remote.")
	return 0
}

func backupStatus(root string) int {
	count := flowbackup.CommitCount(root)
	fmt.Printf("flow root:     %s\n", root)
	fmt.Printf("checkpoints:   %d\n", count)
	if commits, err := flowbackup.Log(root, "", 1); err == nil && len(commits) > 0 {
		c := commits[0]
		fmt.Printf("last:          %s  %s — %s\n", c.Short, c.When, c.Subject)
	} else if count == 0 {
		fmt.Println("last:          (no checkpoints yet — backups start on first KB write or `flow backup now`)")
	}
	fmt.Printf("db snapshots:  %d\n", flowbackup.DBSnapshotCount(root))
	if latest := flowbackup.LatestDBSnapshot(root); latest != "" {
		fmt.Printf("latest db:     %s\n", filepath.Base(latest))
	}
	st := flowbackup.LoadSchedState(root)
	if st.Schedule != "" || st.LastRunAt != "" || st.NextRunAt != "" {
		sched := st.Schedule
		if sched == "" {
			sched = "(default)"
		}
		fmt.Printf("schedule:      %s\n", sched)
		if st.LastRunAt != "" {
			fmt.Printf("last run:      %s\n", st.LastRunAt)
		}
		if st.NextRunAt != "" {
			fmt.Printf("next run:      %s\n", st.NextRunAt)
		}
	}
	if url := flowbackup.RemoteURL(root); url != "" {
		fmt.Printf("offsite:       %s\n", url)
		if st.LastPushAt != "" {
			fmt.Printf("last push:     %s\n", st.LastPushAt)
		}
	} else if login := flowbackup.GitHubLogin(); login != "" {
		fmt.Printf("offsite:       (auto — will use a private GitHub repo under %s on next backup)\n", login)
	} else {
		fmt.Println("offsite:       (local only — authenticate `gh` for automatic private GitHub backup, or set a remote)")
	}
	return 0
}

func backupList(root string, args []string) int {
	fs := flagSet("backup list")
	limit := fs.Int("limit", 50, "max checkpoints to show (0 = all)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	relpath := ""
	if fs.NArg() > 0 {
		relpath = fs.Arg(0)
	}
	commits, err := flowbackup.Log(root, relpath, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if len(commits) == 0 {
		fmt.Println("(no checkpoints)")
		return 0
	}
	for _, c := range commits {
		fmt.Printf("%s  %s  %s\n", c.Short, c.When, c.Subject)
	}
	return 0
}

func backupShow(root string, args []string) int {
	fs := flagSet("backup show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "error: show requires <rev> <relpath>")
		return 2
	}
	body, err := flowbackup.Show(root, fs.Arg(0), fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	_, _ = os.Stdout.Write(body)
	return 0
}

func backupDiff(root string, args []string) int {
	fs := flagSet("backup diff")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "error: diff requires <rev> <relpath>")
		return 2
	}
	out, err := flowbackup.Diff(root, fs.Arg(0), fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if out == "" {
		fmt.Println("(no differences)")
		return 0
	}
	fmt.Print(out)
	return 0
}

func backupRestore(root string, args []string) int {
	fs := flagSet("backup restore")
	at := fs.String("at", "", "revision to restore from (default: the previous version)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "error: restore requires <relpath> (e.g. kb/org.md)")
		return 2
	}
	relpath := fs.Arg(0)
	rev := *at
	if rev == "" {
		// Default: the most recent checkpoint whose content DIFFERS from the
		// current on-disk file. This does the intuitive thing whether or not the
		// bad change was itself checkpointed — after an un-committed wipe it
		// restores the latest good version; after a committed bad change it skips
		// past the matching commit to the last good one.
		commits, err := flowbackup.Log(root, relpath, 0)
		if err != nil || len(commits) == 0 {
			fmt.Fprintf(os.Stderr, "error: no backup history for %s\n", relpath)
			return 1
		}
		cur, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(relpath)))
		for _, c := range commits {
			old, err := flowbackup.Show(root, c.Rev, relpath)
			if err != nil {
				continue
			}
			if !bytes.Equal(old, cur) {
				rev = c.Rev
				break
			}
		}
		if rev == "" {
			fmt.Printf("%s already matches every backed-up version; nothing to restore.\n", relpath)
			return 0
		}
	}
	if err := flowbackup.Restore(root, relpath, rev); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("Restored %s from %s\n", relpath, rev[:min(12, len(rev))])
	return 0
}

func backupNow(root string) int {
	committed, err := flowbackup.Checkpoint(root, "manual backup")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: checkpoint: %v\n", err)
		return 1
	}
	if committed {
		fmt.Println("Checkpoint committed.")
	} else {
		fmt.Println("No markdown changes since the last checkpoint.")
	}
	snap, err := flowbackup.SnapshotDB(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: db snapshot failed: %v\n", err)
	} else if snap != "" {
		fmt.Printf("Wrote db snapshot %s\n", filepath.Base(snap))
	}
	return 0
}
