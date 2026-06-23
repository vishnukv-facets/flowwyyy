package flowclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeVersionFlow(t *testing.T, json string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flow")
	body := "#!/bin/sh\nprintf '%s\\n' '" + json + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckCompatRejectsBelowFloor(t *testing.T) {
	bin := fakeVersionFlow(t, `{"version":"v1.2.2","schema":4,"capabilities":["version-json"]}`)

	err := CheckCompat(context.Background(), bin, Version{Version: "v1.2.3", Schema: 5})
	if err == nil {
		t.Fatal("CheckCompat err = nil, want rejection")
	}
	if msg := err.Error(); !strings.Contains(msg, "v1.2.3") || !strings.Contains(msg, "schema 5") {
		t.Fatalf("compat error %q does not name required floor", msg)
	}
}

func TestCheckCompatAcceptsMeetingFloor(t *testing.T) {
	bin := fakeVersionFlow(t, `{"version":"v1.2.3","schema":5,"capabilities":["version-json"]}`)

	if err := CheckCompat(context.Background(), bin, Version{Version: "v1.2.3", Schema: 5}); err != nil {
		t.Fatal(err)
	}
}
