package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flow/internal/flowdb"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnersAPIListAndDetail(t *testing.T) {
	root, db := testRootDB(t)
	now := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := flowdb.CreateOwner(db, &flowdb.Owner{
		Slug:       "release-watcher",
		Name:       "Release Watcher",
		WorkDir:    root,
		Status:     "active",
		Every:      "1h",
		NextWakeAt: sql.NullString{String: now, Valid: true},
		Harness:    "claude",
	}); err != nil {
		t.Fatal(err)
	}

	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, Version: "test"}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/owners", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var owners []OwnerView
	if err := json.Unmarshal(rec.Body.Bytes(), &owners); err != nil {
		t.Fatal(err)
	}
	if len(owners) != 1 || owners[0].Slug != "release-watcher" || !owners[0].NextDue {
		t.Fatalf("owners = %+v", owners)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/owners/release-watcher", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var owner OwnerView
	if err := json.Unmarshal(rec.Body.Bytes(), &owner); err != nil {
		t.Fatal(err)
	}
	if owner.Name != "Release Watcher" || owner.Harness != "claude" || owner.CharterPath == "" {
		t.Fatalf("owner detail = %+v", owner)
	}
}

func TestOwnersAPILifecycle(t *testing.T) {
	root, db := testRootDB(t)
	if err := flowdb.CreateOwner(db, &flowdb.Owner{
		Slug:    "deploy-owner",
		Name:    "Deploy Owner",
		WorkDir: root,
		Status:  "active",
		Every:   "30m",
		Harness: "claude",
	}); err != nil {
		t.Fatal(err)
	}

	argFile := filepath.Join(root, "owner-tick-args.txt")
	flowScript := filepath.Join(root, "flow-test")
	if err := os.WriteFile(flowScript, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > "+argFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := authedTestHandler(New(Config{DB: db, FlowRoot: root, Version: "test", CommandPath: flowScript}))
	post := func(path, body string) OwnerView {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
		var owner OwnerView
		if err := json.Unmarshal(rec.Body.Bytes(), &owner); err != nil {
			t.Fatal(err)
		}
		return owner
	}

	assertArgs := func(want string) {
		t.Helper()
		got, err := os.ReadFile(argFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want+"\n" {
			t.Fatalf("delegated args = %q, want %q", string(got), want)
		}
	}

	// Owner lifecycle actions are Bucket O — the server delegates the write to the
	// `flow owner` CLI (asserted via the recorded args) rather than mutating the
	// owners table directly (seam §11). `owner start` = ActivateOwner(now);
	// `owner next --at` = SetOwnerNextWake(explicit time).
	post("/api/owners/deploy-owner/pause", `{}`)
	assertArgs("owner pause deploy-owner")
	post("/api/owners/deploy-owner/start", `{}`)
	assertArgs("owner start deploy-owner")
	next := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
	post("/api/owners/deploy-owner/next", `{"at":"`+next+`"}`)
	assertArgs("owner next deploy-owner --at " + next)
	post("/api/owners/deploy-owner/retire", `{}`)
	assertArgs("owner retire deploy-owner")
	post("/api/owners/deploy-owner/tick", `{}`)
	assertArgs("owner tick deploy-owner --auto")
}
