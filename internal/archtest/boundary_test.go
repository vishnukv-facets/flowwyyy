// Package archtest enforces the flow-core ↔ flowwyyy-product dependency
// boundary as a test-time fitness function: no core package may import a
// product package (server/monitor/steering).
//
// It uses a ratchet (knownViolations): packages that currently violate the
// rule are allowlisted, each decoupling task removes entries, and the list
// ends empty. The test fails in BOTH directions — on a new violation
// (regression) and when an allowlisted package becomes clean (so the stale
// entry gets removed). See docs/architecture/flow-core-decoupling-plan.md.
package archtest

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

var productPkgs = map[string]bool{
	"flow/internal/server":   true,
	"flow/internal/monitor":  true,
	"flow/internal/steering": true,
}

// knownViolations is the ratchet; each task removes entries; ends empty.
// Empty: ui/attention/slack relocated to product (T6), tell+init+workevents
// rewired to core inbox/init-hooks (T4/T5). Core no longer imports product.
var knownViolations = map[string]bool{}

var corePackages = []string{
	"flow/internal/app", "flow/internal/flowdb", "flow/internal/workevents",
	"flow/internal/briefing", "flow/internal/agents", "flow/internal/agenthooks",
	"flow/internal/worktree", "flow/internal/workdirreg", "flow/internal/memorysrc",
	"flow/internal/schedule", "flow/internal/flowbackup", "flow/internal/listfmt",
	"flow/internal/spawner", "flow/internal/termutil", "flow/internal/ghpr", "flow/internal/ghref",
}

func deps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	return strings.Fields(string(out))
}

// productBinaryPkgs are packages the core `flow` binary must never pull in:
// the product surface plus its DB layers. Importing any of them would mean the
// core binary carries Mission Control / connector / steering code or creates
// product tables — defeating the two-binary split.
var productBinaryPkgs = map[string]bool{
	"flow/internal/server":    true,
	"flow/internal/monitor":   true,
	"flow/internal/steering":  true,
	"flow/internal/product":   true,
	"flow/internal/productdb": true,
}

// TestCoreBinaryStaysProductFree asserts cmd/flow (the core engine binary) does
// not transitively import any product package. This keeps the core binary
// independently shippable and ensures `flow init` opens a core-only schema (no
// product tables), which the flowwyyy binary owns and migrates separately.
func TestCoreBinaryStaysProductFree(t *testing.T) {
	for _, d := range deps(t, "flow/cmd/flow") {
		if productBinaryPkgs[d] {
			t.Errorf("cmd/flow (core binary) imports product package %s — it must stay product-free", d)
		}
	}
}

func TestCoreDoesNotImportProduct(t *testing.T) {
	var clean []string
	for _, pkg := range corePackages {
		bad := false
		for _, d := range deps(t, pkg) {
			if productPkgs[d] {
				bad = true
				break
			}
		}
		switch {
		case bad && !knownViolations[pkg]:
			t.Errorf("REGRESSION: core package %s imports a product package", pkg)
		case !bad && knownViolations[pkg]:
			clean = append(clean, pkg)
		}
	}
	if len(clean) > 0 {
		sort.Strings(clean)
		t.Errorf("clean now — remove from knownViolations: %v", clean)
	}
}

// coreGoPkgs are the core engine's Go packages. By Phase 3, official
// Facets-cloud/flow keeps these under internal/, so the flowwyyy product binary
// can never import them: it must read the shared ~/.flow DB through its OWN
// layer (internal/productdb) and exec the `flow` binary for core mutations.
var coreGoPkgs = map[string]bool{
	"flow/internal/app":    true,
	"flow/internal/flowdb": true,
}

// productGoPkgs are the flowwyyy-product packages + the product binary that must
// not transitively import core Go (coreGoPkgs). productdb is included because it
// is flowwyyy's OWN shared-DB layer: if it imports flowdb, every consumer that
// reaches the DB through it also transitively pulls flowdb. Checked
// transitively, so making productdb flowdb-free is a prerequisite for cutting
// any consumer over to it.
var productGoPkgs = []string{
	"flow/cmd/flowwyyy",
	"flow/internal/server",
	"flow/internal/monitor",
	"flow/internal/steering",
	"flow/internal/product",
	"flow/internal/productdb",
}

// productImportsCoreGo is the SECOND ratchet (plan T13): product packages that
// still import core Go (app/flowdb). Each burndown step ports a package's reads
// onto productdb and removes it here; the list ends empty when flowwyyy owns its
// read layer end to end. Like knownViolations, the test fails in both
// directions — on a new violation and when a ratcheted package becomes clean.
var productImportsCoreGo = map[string]bool{
	"flow/cmd/flowwyyy":      true, // app.Version + transitive flowdb via product/server
	"flow/internal/server":   true, // reads/writes via flowdb directly (51 non-test files)
	"flow/internal/monitor":  true, // task/project reads + github/slack writes via flowdb
	"flow/internal/steering": true, // attention_feed/steering_trace reads+writes via flowdb
	"flow/internal/product":  true, // attention.go/ui.go read via flowdb
}

// TestProductDoesNotImportCoreGo enforces the Phase-3 boundary: the flowwyyy
// product surface reads via productdb and execs `flow` for writes, never
// importing flow/internal/app or flow/internal/flowdb.
func TestProductDoesNotImportCoreGo(t *testing.T) {
	var clean []string
	for _, pkg := range productGoPkgs {
		bad := false
		for _, d := range deps(t, pkg) {
			if coreGoPkgs[d] {
				bad = true
				break
			}
		}
		switch {
		case bad && !productImportsCoreGo[pkg]:
			t.Errorf("REGRESSION: product package %s imports core Go (app/flowdb) — it must read via productdb and exec flow for writes", pkg)
		case !bad && productImportsCoreGo[pkg]:
			clean = append(clean, pkg)
		}
	}
	if len(clean) > 0 {
		sort.Strings(clean)
		t.Errorf("clean now — remove from productImportsCoreGo ratchet: %v", clean)
	}
}
