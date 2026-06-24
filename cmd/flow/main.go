// Command flow is the core engine + CLI: tasks, projects, playbooks, owners,
// sessions, search, backup. It registers ONLY core verbs (via app.init →
// registerCore) and contains no product code — Mission Control, connectors, and
// the Attention Router live in the separate flowwyyy binary. Keeping this binary
// product-free is enforced by internal/archtest.
package main

import (
	"os"

	"flow/internal/app"
)

// version is set at build time via `-ldflags -X main.version=<tag>`. "dev"
// means an unversioned local build.
var version = "dev"

func main() {
	app.Version = version
	os.Exit(app.Run(os.Args[1:]))
}
