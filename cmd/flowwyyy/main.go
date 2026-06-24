// Command flowwyyy is the product binary: Mission Control (web UI + terminal
// bridge), the Slack/GitHub connectors, and the Attention Router/steering. It
// consumes the core `flow` engine as an external versioned binary — execing it
// by absolute path for every mutation/launch and reading the shared ~/.flow DB
// directly. Product verbs (ui serve, attention, slack) run in-process; every
// other verb passes through to `flow`, preserving the full command surface.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"flow/internal/flowclient"
	"flow/internal/product"
)

// version is set at build time via `-ldflags -X main.version=<tag>`.
var version = "dev"

func main() {
	// Product commands (ui serve) stamp this into Mission Control's version
	// display, so the product binary's version is what the UI shows.
	product.Version = version

	bin, err := flowclient.Resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := flowclient.CheckCompat(ctx, bin, flowclient.RequiredFloor); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(product.RunWithPassthrough(bin, os.Args[1:]))
}
