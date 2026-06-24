package productdb_test

// Cross-layer parity for the hand-written extractions in productdb (normalizers,
// owners/playbooks/workdirs reads, tag aggregates, task-by-session, and the
// task-start blocker). The whole-file Bucket-F ports are byte-identical to
// flowdb, but these were transcribed by hand, so each is pinned against its
// flowdb twin: seed via flowdb (the core writer), read via productdb, assert
// identical results.

import (
	"testing"

	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func TestNormalizerParity(t *testing.T) {
	if productdb.DefaultPermissionMode != flowdb.DefaultPermissionMode {
		t.Fatalf("DefaultPermissionMode: productdb=%q flowdb=%q", productdb.DefaultPermissionMode, flowdb.DefaultPermissionMode)
	}
	for _, in := range []string{"", "default", "auto", "bypass", "dangerously-skip-permissions", "garbage"} {
		fm, fe := flowdb.NormalizePermissionMode(in)
		pm, pe := productdb.NormalizePermissionMode(in)
		if fm != pm || (fe == nil) != (pe == nil) {
			t.Errorf("NormalizePermissionMode(%q): flowdb=(%q,%v) productdb=(%q,%v)", in, fm, fe, pm, pe)
		}
	}
	for _, in := range []string{"", "high", "h", "medium", "m", "low", "l", "nope"} {
		fp, fe := flowdb.NormalizePriority(in)
		pp, pe := productdb.NormalizePriority(in)
		if fp != pp || (fe == nil) != (pe == nil) {
			t.Errorf("NormalizePriority(%q): flowdb=(%q,%v) productdb=(%q,%v)", in, fp, fe, pp, pe)
		}
	}
	for _, in := range []string{"", "claude", "claude-code", "codex", "codex-cli", "bogus"} {
		fp, fe := flowdb.NormalizeSessionProvider(in)
		pp, pe := productdb.NormalizeSessionProvider(in)
		if fp != pp || (fe == nil) != (pe == nil) {
			t.Errorf("NormalizeSessionProvider(%q): flowdb=(%q,%v) productdb=(%q,%v)", in, fp, fe, pp, pe)
		}
		fh, fhe := flowdb.NormalizeHarnessName(in)
		ph, phe := productdb.NormalizeHarnessName(in)
		if fh != ph || (fhe == nil) != (phe == nil) {
			t.Errorf("NormalizeHarnessName(%q): flowdb=(%q,%v) productdb=(%q,%v)", in, fh, fhe, ph, phe)
		}
	}
}

func TestModelResolutionParity(t *testing.T) {
	for _, m := range []string{"", "opus", "Sonnet", "  gpt-5.4  "} {
		if flowdb.NormalizeModel(m) != productdb.NormalizeModel(m) {
			t.Errorf("NormalizeModel(%q): flowdb=%q productdb=%q", m, flowdb.NormalizeModel(m), productdb.NormalizeModel(m))
		}
	}
	cases := []struct{ provider, explicit, brief, priority string }{
		{"claude", "", "", "high"},
		{"claude", "opus", "short brief", "low"},
		{"codex", "", "A long descriptive brief about doing the work.\nDone when: a\nDone when: b", "medium"},
		{"codex", "", "", "high"},
	}
	for _, c := range cases {
		f := flowdb.ResolveSessionModel(c.provider, c.explicit, c.brief, c.priority)
		p := productdb.ResolveSessionModel(c.provider, c.explicit, c.brief, c.priority)
		if f.Model != p.Model {
			t.Errorf("ResolveSessionModel(%+v).Model: flowdb=%q productdb=%q", c, f.Model, p.Model)
		}
	}
}

func TestOwnerReadParity(t *testing.T) {
	db := openSeeded(t)
	if err := flowdb.CreateOwner(db, &flowdb.Owner{
		Slug: "ops", Name: "Ops Owner", WorkDir: "/tmp/ops", Status: "active",
		Every: "every 6 hours", Harness: "codex",
	}); err != nil {
		t.Fatalf("CreateOwner: %v", err)
	}
	f, fe := flowdb.GetOwner(db, "ops")
	p, pe := productdb.GetOwner(db, "ops")
	if fe != nil || pe != nil {
		t.Fatalf("GetOwner err: flowdb=%v productdb=%v", fe, pe)
	}
	if f.Slug != p.Slug || f.Name != p.Name || f.Status != p.Status || f.Every != p.Every || f.Harness != p.Harness {
		t.Errorf("GetOwner parity: flowdb=%+v productdb=%+v", f, p)
	}
	fl, _ := flowdb.ListOwners(db, flowdb.OwnerFilter{})
	pl, _ := productdb.ListOwners(db, productdb.OwnerFilter{})
	if len(fl) != len(pl) || len(pl) != 1 {
		t.Errorf("ListOwners count: flowdb=%d productdb=%d (want 1)", len(fl), len(pl))
	}
}

func TestPlaybookReadParity(t *testing.T) {
	db := openSeeded(t)
	if err := flowdb.UpsertPlaybook(db, &flowdb.Playbook{
		Slug: "daily", Name: "Daily Sweep", WorkDir: "/tmp/daily",
	}); err != nil {
		t.Fatalf("UpsertPlaybook: %v", err)
	}
	f, fe := flowdb.GetPlaybook(db, "daily")
	p, pe := productdb.GetPlaybook(db, "daily")
	if fe != nil || pe != nil {
		t.Fatalf("GetPlaybook err: flowdb=%v productdb=%v", fe, pe)
	}
	if f.Slug != p.Slug || f.Name != p.Name || f.WorkDir != p.WorkDir {
		t.Errorf("GetPlaybook parity: flowdb=%+v productdb=%+v", f, p)
	}
	fl, _ := flowdb.ListPlaybooks(db, flowdb.PlaybookFilter{})
	pl, _ := productdb.ListPlaybooks(db, productdb.PlaybookFilter{})
	if len(fl) != len(pl) || len(pl) != 1 {
		t.Errorf("ListPlaybooks count: flowdb=%d productdb=%d (want 1)", len(fl), len(pl))
	}
}

func TestWorkdirReadParity(t *testing.T) {
	db := openSeeded(t)
	if err := flowdb.UpsertWorkdir(db, "/tmp/repo", "repo", "a repo", "git@github.com:o/r.git"); err != nil {
		t.Fatalf("UpsertWorkdir: %v", err)
	}
	f, fe := flowdb.GetWorkdir(db, "/tmp/repo")
	p, pe := productdb.GetWorkdir(db, "/tmp/repo")
	if fe != nil || pe != nil {
		t.Fatalf("GetWorkdir err: flowdb=%v productdb=%v", fe, pe)
	}
	if f.Path != p.Path || f.Name.String != p.Name.String || f.GitRemote.String != p.GitRemote.String {
		t.Errorf("GetWorkdir parity: flowdb=%+v productdb=%+v", f, p)
	}
	fl, _ := flowdb.ListWorkdirs(db)
	pl, _ := productdb.ListWorkdirs(db)
	if len(fl) != len(pl) || len(pl) != 1 {
		t.Errorf("ListWorkdirs count: flowdb=%d productdb=%d (want 1)", len(fl), len(pl))
	}
}

func TestTagAggregateAndSessionParity(t *testing.T) {
	db := openSeeded(t)
	seedTask(t, db, "alpha", "Alpha", "", "backlog", "high", "claude")
	seedTask(t, db, "beta", "Beta", "", "in-progress", "medium", "claude")
	if err := flowdb.AddTaskTag(db, "alpha", "infra"); err != nil {
		t.Fatalf("AddTaskTag alpha: %v", err)
	}
	if err := flowdb.AddTaskTag(db, "beta", "infra"); err != nil {
		t.Fatalf("AddTaskTag beta: %v", err)
	}
	fl, _ := flowdb.ListAllTags(db)
	pl, _ := productdb.ListAllTags(db)
	if len(fl) != len(pl) || len(pl) != 1 || pl[0].Tag != "infra" || pl[0].Count != 2 {
		t.Errorf("ListAllTags parity: flowdb=%+v productdb=%+v", fl, pl)
	}

	// TaskBySessionID: beta is in-progress so seedTask gave it session_id beta-session.
	f, fe := flowdb.TaskBySessionID(db, "beta-session")
	p, pe := productdb.TaskBySessionID(db, "beta-session")
	if fe != nil || pe != nil {
		t.Fatalf("TaskBySessionID err: flowdb=%v productdb=%v", fe, pe)
	}
	if f.Slug != p.Slug || p.Slug != "beta" {
		t.Errorf("TaskBySessionID parity: flowdb=%q productdb=%q", f.Slug, p.Slug)
	}
}

func TestBlockerParity(t *testing.T) {
	db := openSeeded(t)
	seedTask(t, db, "parent", "Parent", "", "backlog", "high", "claude")
	seedTask(t, db, "child", "Child", "", "backlog", "high", "claude")
	if err := flowdb.AddTaskDependency(db, "child", "parent"); err != nil {
		t.Fatalf("AddTaskDependency: %v", err)
	}
	child, _ := productdb.GetTask(db, "child")
	flowChild, _ := flowdb.GetTask(db, "child")

	// While parent is not done, the child is blocked — both layers agree it is
	// non-nil and produce the same Error() text.
	fb, _ := flowdb.TaskStartBlockerFor(db, flowChild)
	pb, _ := productdb.TaskStartBlockerFor(db, child)
	if (fb == nil) != (pb == nil) {
		t.Fatalf("blocker presence mismatch: flowdb=%v productdb=%v", fb, pb)
	}
	if fb != nil && fb.Error() != pb.Error() {
		t.Errorf("blocker Error() mismatch:\n flowdb=%q\n productdb=%q", fb.Error(), pb.Error())
	}
	if err := productdb.EnsureTaskStartable(db, child); err == nil {
		t.Errorf("EnsureTaskStartable: expected block while parent not done")
	}

	// Complete the parent → child becomes startable in both layers.
	if _, err := db.Exec(`UPDATE tasks SET status='done' WHERE slug='parent'`); err != nil {
		t.Fatalf("complete parent: %v", err)
	}
	if err := productdb.EnsureTaskStartable(db, child); err != nil {
		t.Errorf("EnsureTaskStartable after parent done: unexpected block %v", err)
	}
}
