package steering

import (
	"database/sql"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func taskImpactDB(t *testing.T) (*sql.DB, string) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("FLOW_ROOT", root)
	db, err := flowdb.OpenDB(filepath.Join(root, "flow.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	return db, root
}

func seedImpactTask(t *testing.T, db *sql.DB, slug, name, status, priority, waitingOn, assignee string) {
	t.Helper()

	now := "2026-06-07T10:00:00Z"
	sessionID := ""
	if status != "backlog" {
		sessionID = fakeImpactSessionID(slug)
	}
	if _, err := db.Exec(
		`INSERT INTO tasks (slug, name, status, kind, priority, work_dir, waiting_on, assignee, session_provider, session_id, created_at, updated_at)
		 VALUES (?, ?, ?, 'regular', ?, ?, ?, ?, 'codex', ?, ?, ?)`,
		slug, name, status, priority, t.TempDir(), productdb.NullIfEmpty(waitingOn), productdb.NullIfEmpty(assignee), productdb.NullIfEmpty(sessionID), now, now,
	); err != nil {
		t.Fatalf("seed task %s: %v", slug, err)
	}
}

func fakeImpactSessionID(slug string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(slug))
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", h.Sum32())
}

func TestBuildTaskImpactHintsMatchesWaitingOnPerson(t *testing.T) {
	db, _ := taskImpactDB(t)
	defer db.Close()

	seedImpactTask(t, db, "raptor-review", "Raptor PR review", "in-progress", "high", "Rohit review on PR #159", "")
	seedImpactTask(t, db, "unrelated-task", "Unrelated task", "in-progress", "medium", "Anshul approval on rollout", "")

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{
			"Rohit Raveendran",
		},
		Text: "Rohit is on leave tomorrow",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 1 {
		t.Fatalf("len(hints) = %d, want 1: %+v", len(hints), hints)
	}
	hint := hints[0]
	if hint.TaskSlug != "raptor-review" {
		t.Errorf("TaskSlug = %q, want raptor-review", hint.TaskSlug)
	}
	if hint.Strength != "strong" {
		t.Errorf("Strength = %q, want strong", hint.Strength)
	}
	if hint.Reason == "" {
		t.Errorf("Reason is empty")
	}
	if hint.Evidence == "" {
		t.Errorf("Evidence is empty")
	}
}

func TestBuildTaskImpactHintsIncludesArchivedOpenTasks(t *testing.T) {
	db, _ := taskImpactDB(t)
	defer db.Close()

	seedImpactTask(t, db, "archived-review", "Archived Raptor review", "in-progress", "high", "Rohit review on PR #159", "")
	if _, err := db.Exec(`UPDATE tasks SET archived_at=? WHERE slug='archived-review'`, "2026-06-07T10:30:00Z"); err != nil {
		t.Fatalf("archive task: %v", err)
	}

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{
			"Rohit Raveendran",
		},
		Text: "Rohit is on leave tomorrow",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 1 {
		t.Fatalf("len(hints) = %d, want 1: %+v", len(hints), hints)
	}
	if hints[0].TaskSlug != "archived-review" {
		t.Errorf("TaskSlug = %q, want archived-review", hints[0].TaskSlug)
	}
}

func TestBuildTaskImpactHintsMatchesAssigneeAndTaskName(t *testing.T) {
	t.Run("assignee strong", func(t *testing.T) {
		db, _ := taskImpactDB(t)
		defer db.Close()

		seedImpactTask(t, db, "anshul-review", "Anshul review on Facets-cloud/raptor#159", "in-progress", "high", "", "Anshul Sao")

		hints, err := BuildTaskImpactHints(db, TaskImpactInput{
			Source: "slack",
			People: []string{
				"Anshul Sao",
			},
			Text: "I can review this after Monday.",
		})
		if err != nil {
			t.Fatalf("BuildTaskImpactHints: %v", err)
		}
		if len(hints) != 1 {
			t.Fatalf("len(hints) = %d, want 1: %+v", len(hints), hints)
		}
		if hints[0].TaskSlug != "anshul-review" {
			t.Errorf("TaskSlug = %q, want anshul-review", hints[0].TaskSlug)
		}
		if hints[0].Strength != "strong" {
			t.Errorf("Strength = %q, want strong", hints[0].Strength)
		}
	})

	t.Run("task name medium", func(t *testing.T) {
		db, _ := taskImpactDB(t)
		defer db.Close()

		seedImpactTask(t, db, "anshul-name-review", "Anshul Sao review on Facets-cloud/raptor#159", "in-progress", "high", "", "")

		hints, err := BuildTaskImpactHints(db, TaskImpactInput{
			Source: "slack",
			People: []string{
				"Anshul Sao",
			},
			Text: "I can review this after Monday.",
		})
		if err != nil {
			t.Fatalf("BuildTaskImpactHints: %v", err)
		}
		if len(hints) != 1 {
			t.Fatalf("len(hints) = %d, want 1: %+v", len(hints), hints)
		}
		if hints[0].TaskSlug != "anshul-name-review" {
			t.Errorf("TaskSlug = %q, want anshul-name-review", hints[0].TaskSlug)
		}
		if hints[0].Strength != "medium" {
			t.Errorf("Strength = %q, want medium", hints[0].Strength)
		}
	})
}

func TestBuildTaskImpactHintsIgnoresWeakCommonTokens(t *testing.T) {
	db, _ := taskImpactDB(t)
	defer db.Close()

	seedImpactTask(t, db, "review-task", "Review task", "in-progress", "high", "external review", "")

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{
			"Review",
		},
		Text: "Review is unavailable today",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("len(hints) = %d, want 0: %+v", len(hints), hints)
	}
}

func TestBuildTaskImpactHintsMatchesTaskTag(t *testing.T) {
	db, _ := taskImpactDB(t)
	defer db.Close()

	seedImpactTask(t, db, "rohit-tagged", "Raptor PR follow-up", "in-progress", "medium", "", "")
	if err := flowdb.AddTaskTag(db, "rohit-tagged", "rohit-raveendran"); err != nil {
		t.Fatalf("AddTaskTag: %v", err)
	}

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{
			"Rohit Raveendran",
		},
		Text: "Rohit is out today",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 1 {
		t.Fatalf("len(hints) = %d, want 1: %+v", len(hints), hints)
	}
	if hints[0].TaskSlug != "rohit-tagged" {
		t.Errorf("TaskSlug = %q, want rohit-tagged", hints[0].TaskSlug)
	}
	if hints[0].Strength != "medium" {
		t.Errorf("Strength = %q, want medium", hints[0].Strength)
	}
}

func TestBuildTaskImpactHintsSkipsCommonSingleTokenPeople(t *testing.T) {
	tests := []struct {
		name      string
		person    string
		waitingOn string
	}{
		{
			name:      "will modal",
			person:    "Will",
			waitingOn: "Anshul will review",
		},
		{
			name:      "may month modal",
			person:    "May",
			waitingOn: "May availability review",
		},
		{
			name:      "non distinctive first name",
			person:    "Alex Smith",
			waitingOn: "Alex review on the rollout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _ := taskImpactDB(t)
			defer db.Close()

			seedImpactTask(t, db, "review-task", "Review task", "in-progress", "high", tt.waitingOn, "")

			hints, err := BuildTaskImpactHints(db, TaskImpactInput{
				Source: "slack",
				People: []string{
					tt.person,
				},
				Text: tt.person + " is unavailable",
			})
			if err != nil {
				t.Fatalf("BuildTaskImpactHints: %v", err)
			}
			if len(hints) != 0 {
				t.Fatalf("len(hints) = %d, want 0: %+v", len(hints), hints)
			}
		})
	}
}

func TestBuildTaskImpactHintsMatchesFullDisplayNamePhrase(t *testing.T) {
	db, _ := taskImpactDB(t)
	defer db.Close()

	seedImpactTask(t, db, "may-lee-review", "May Lee review", "in-progress", "high", "May Lee review on the rollout", "")

	hints, err := BuildTaskImpactHints(db, TaskImpactInput{
		Source: "slack",
		People: []string{
			"May Lee",
		},
		Text: "May Lee is unavailable today",
	})
	if err != nil {
		t.Fatalf("BuildTaskImpactHints: %v", err)
	}
	if len(hints) != 1 {
		t.Fatalf("len(hints) = %d, want 1: %+v", len(hints), hints)
	}
	if hints[0].TaskSlug != "may-lee-review" {
		t.Errorf("TaskSlug = %q, want may-lee-review", hints[0].TaskSlug)
	}
}
