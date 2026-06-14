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

	if o := post("/api/owners/deploy-owner/pause", `{}`); o.Status != "paused" {
		t.Fatalf("after pause = %+v", o)
	}
	if o := post("/api/owners/deploy-owner/start", `{}`); o.Status != "active" || o.NextWakeAt == nil {
		t.Fatalf("after start = %+v", o)
	}
	next := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
	if o := post("/api/owners/deploy-owner/next", `{"at":"`+next+`"}`); o.NextWakeAt == nil || *o.NextWakeAt != next {
		t.Fatalf("after next = %+v, want %s", o, next)
	}
	if o := post("/api/owners/deploy-owner/retire", `{}`); o.Status != "retired" || o.ArchivedAt == nil {
		t.Fatalf("after retire = %+v", o)
	}
	if _, err := db.Exec(`UPDATE owners SET status='active', archived_at=NULL WHERE slug='deploy-owner'`); err != nil {
		t.Fatal(err)
	}
	_ = post("/api/owners/deploy-owner/tick", `{}`)
	got, err := os.ReadFile(argFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "owner tick deploy-owner --auto\n" {
		t.Fatalf("tick args = %q", got)
	}
}
