package app

import (
	"bytes"
	"compress/gzip"
	"flow/internal/flowbackup"
	"flow/internal/flowdb"
	"flow/internal/steering"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// flowRoot returns the root directory for flow state. Honors $FLOW_ROOT if
// set, otherwise falls back to ~/.flow. The returned path is not guaranteed
// to exist — callers that need it created should run `flow init` or create
// it themselves.
func flowRoot() (string, error) {
	if r := os.Getenv("FLOW_ROOT"); r != "" {
		return r, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home dir: %w", err)
	}
	return filepath.Join(home, ".flow"), nil
}

// flowDBPath returns the absolute path to flow.db under the flow root.
// Returns a clear error if the data directory hasn't been initialized.
func flowDBPath() (string, error) {
	root, err := flowRoot()
	if err != nil {
		return "", err
	}
	dbPath := filepath.Join(root, "flow.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return "", fmt.Errorf("flow is not initialized — run `flow init` to set up %s", root)
	}
	return dbPath, nil
}

// cmdInit creates ~/.flow/ (or $FLOW_ROOT), initializes flow.db, installs
// the skill. Idempotent — re-running is safe.
func cmdInit(args []string) int {
	fs := flagSet("init")
	restoreFrom := fs.String("restore-from", "", "restore an existing backup from a git remote into the flow root (new-laptop setup)")
	force := fs.Bool("force", false, "with --restore-from: proceed even if the flow root is non-empty")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "error: init takes no positional arguments")
		return 2
	}

	root, err := flowRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// New-laptop restore: clone the curated markdown + restore the db snapshot
	// before the normal init steps (which then run idempotently over the
	// restored tree).
	if strings.TrimSpace(*restoreFrom) != "" {
		if rc := restoreFromRemote(root, *restoreFrom, *force); rc != 0 {
			return rc
		}
	}

	// Create the top-level tree. `projects/` and `tasks/` are parents for
	// per-project and per-task subdirectories created later by `flow add`.
	// `kb/` holds 5 knowledge-base files (user/org/products/processes/
	// business) that the skill appends to on the fly and that `flow show
	// task`/`show project` lists for execution sessions to read.
	for _, d := range []string{
		root,
		filepath.Join(root, "projects"),
		filepath.Join(root, "tasks"),
		filepath.Join(root, "kb"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "error: mkdir %s: %v\n", d, err)
			return 1
		}
	}

	// Seed the KB files. Idempotent: only create if missing; never
	// overwrite existing content.
	for _, kb := range kbSeeds() {
		path := filepath.Join(root, "kb", kb.filename)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte(kb.stub), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "error: write %s: %v\n", path, err)
				return 1
			}
		}
	}

	// Seed the operator voice/persona (persona.md at the flow root) so outbound
	// Slack/GitHub replies sound human from day one — globally applied and
	// editable. The steerer injects it into drafting + send prompts. Idempotent:
	// only create if missing; never overwrite an edited voice.
	personaPath := filepath.Join(root, "persona.md")
	if _, err := os.Stat(personaPath); os.IsNotExist(err) {
		if err := os.WriteFile(personaPath, []byte(steering.DefaultPersonaMarkdown), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write %s: %v\n", personaPath, err)
			return 1
		}
	}

	// Open (and implicitly initialize) flow.db.
	dbPath := filepath.Join(root, "flow.db")
	db, err := flowdb.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	db.Close()

	// Install the skill idempotently. Skip if already present; we never
	// overwrite on init (use `flow skill update` for that).
	skillPath, err := skillInstallPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		if err := installEmbeddedSkill(skillPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: write %s: %v\n", skillPath, err)
			return 1
		}
		if err := writeSkillVersion(Version); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record skill version: %v\n", err)
		}
		fmt.Printf("installed flow skill to %s\n", skillPath)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "error: stat %s: %v\n", skillPath, err)
		return 1
	}

	// Install the SessionStart hook idempotently.
	if added, err := installSessionStartHook(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install SessionStart hook: %v\n", err)
	} else if added {
		settings, _ := userSettingsPath()
		fmt.Printf("installed SessionStart hook in %s\n", settings)
	}
	if added, err := installClaudeStatusLine(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not install Claude statusLine capture: %v\n", err)
	} else if added {
		settings, _ := userSettingsPath()
		fmt.Printf("installed Claude statusLine capture in %s\n", settings)
	}

	// Initialize the backup safety net: a self-managed git repo over the flow
	// root that versions curated markdown (kb + briefs/updates), plus a baseline
	// db snapshot. Best-effort — never fail init over a backup hiccup.
	if _, err := flowbackup.Checkpoint(root, "flow init"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: backup init: %v\n", err)
	}
	if _, err := flowbackup.SnapshotDB(root); err != nil {
		fmt.Fprintf(os.Stderr, "warning: db snapshot: %v\n", err)
	}

	fmt.Printf("flow initialized at %s\n", root)
	fmt.Println(`Next: flow add project "My first project" --work-dir <path>`)
	return 0
}

// restoreFromRemote performs a new-laptop restore: clone the curated markdown
// from a backup remote into the (empty) flow root, then restore the database
// from the remote's bounded db-snapshot branch and rebuild the search index.
// Returns a non-zero exit code on failure.
func restoreFromRemote(root, url string, force bool) int {
	// Guard against clobbering an existing install. A bare .backupgit/.gitignore
	// from a prior aborted attempt doesn't count as "non-empty".
	if entries, err := os.ReadDir(root); err == nil {
		meaningful := 0
		for _, e := range entries {
			if e.Name() == gitDirNameForRoot || e.Name() == ".gitignore" {
				continue
			}
			meaningful++
		}
		if meaningful > 0 && !force {
			fmt.Fprintf(os.Stderr, "error: %s is not empty; refusing to restore over it. Re-run with --force to overwrite, or restore into a fresh $FLOW_ROOT.\n", root)
			return 1
		}
	}

	fmt.Printf("Restoring backup from %s ...\n", url)
	if err := flowbackup.Clone(root, url); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Println("  ✓ curated markdown (KB + briefs/updates) restored")

	// Restore the database from the bounded db-snapshot branch, if present.
	gz, err := flowbackup.FetchDBSnapshotBytes(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not fetch db snapshot: %v\n", err)
	} else if len(gz) > 0 {
		if err := writeGunzipped(gz, filepath.Join(root, "flow.db")); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore db: %v\n", err)
		} else {
			fmt.Println("  ✓ database restored from snapshot")
			// Rebuild the regenerable search index (excluded from the snapshot).
			if db, err := flowdb.OpenDB(filepath.Join(root, "flow.db")); err == nil {
				if err := flowdb.SyncSearchDocs(db, root, true); err != nil {
					fmt.Fprintf(os.Stderr, "warning: search reindex failed (search will repopulate later): %v\n", err)
				} else {
					fmt.Println("  ✓ search index rebuilt")
				}
				db.Close()
			}
		}
	} else {
		fmt.Println("  • no db snapshot on the remote — starting with an empty task list (knowledge files are restored)")
	}
	return 0
}

// gitDirNameForRoot mirrors flowbackup's separated gitdir name for the
// non-empty-root guard. Kept in sync with flowbackup.gitDirName.
const gitDirNameForRoot = ".backupgit"

// writeGunzipped decompresses gzip data to dst.
func writeGunzipped(gz []byte, dst string) error {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, zr)
	return err
}

// kbSeed describes one knowledge-base file to create on `flow init`.
type kbSeed struct {
	filename string
	stub     string
}

// kbSeeds returns the five canonical KB files. Kept as a function (not a
// package-level var) so tests can call it directly without sharing state.
// The skill is the authoritative source of how these files are appended
// to; this stub text is only visible to a human opening the file before
// any entries have been written.
func kbSeeds() []kbSeed {
	return []kbSeed{
		{"user.md", `# User

Durable facts about the person using flow — role, preferences, working
style, constraints, availability. The flow skill appends entries here
automatically when the user shares something in conversation.

<!-- Entries are appended as: "- YYYY-MM-DD — <short quote or paraphrase>" -->
`},
		{"org.md", `# Organization

The user's company, team, structure, stakeholders, reporting lines, and
people the user interacts with. Appended by the flow skill on the fly.

<!-- Entries are appended as: "- YYYY-MM-DD — <short quote or paraphrase>" -->
`},
		{"products.md", `# Products

What the org ships — product lines, modules, features, release cadence,
major capabilities. Appended by the flow skill on the fly.

<!-- Entries are appended as: "- YYYY-MM-DD — <short quote or paraphrase>" -->
`},
		{"processes.md", `# Processes

How the org works — tools, conventions, rituals, review rules, on-call,
deploy flows, meeting cadences. Appended by the flow skill on the fly.

<!-- Entries are appended as: "- YYYY-MM-DD — <short quote or paraphrase>" -->
`},
		{"business.md", `# Business

Customers, business model, revenue, deals, market positioning, contract
structure, pricing. Appended by the flow skill on the fly.

<!-- Entries are appended as: "- YYYY-MM-DD — <short quote or paraphrase>" -->
`},
	}
}

// kbFiles returns the absolute paths of all existing KB files under the
// given flow root, in the canonical order (user, org, products, processes,
// business). Paths for files that don't exist on disk are omitted — users
// can delete any kb file they don't care about without breaking show.
func kbFiles(root string) []string {
	var out []string
	for _, kb := range kbSeeds() {
		p := filepath.Join(root, "kb", kb.filename)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
