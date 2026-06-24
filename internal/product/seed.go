package product

import (
	"os"
	"path/filepath"

	"flow/internal/cli"
	"flow/internal/steering"
)

// seedSteererPersona writes <root>/persona.md (the steerer's default voice) when
// absent. Idempotent — a no-op once the file exists.
//
// It used to register as an app init-hook that `flow init` ran, but in the
// two-binary world `flow init` runs in the CORE binary, which never imports the
// product package — so the hook would never fire (Phase-3 decoupling, seam
// §11.3.1, Tier D). flowwyyy now seeds it lazily at `ui serve` startup instead
// (see serveUI); being idempotent, calling it on every serve is safe and means
// the persona exists regardless of how/where flow.db was initialized.
func seedSteererPersona() error {
	root, err := cli.FlowRoot()
	if err != nil {
		return err
	}
	p := filepath.Join(root, "persona.md")
	if _, err := os.Stat(p); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(p, []byte(steering.DefaultPersonaMarkdown), 0o644)
}
