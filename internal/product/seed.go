package product

import (
	"os"
	"path/filepath"

	"flow/internal/app"
	"flow/internal/steering"
)

func seedSteererPersona() error {
	root, err := app.FlowRoot()
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

func init() { app.RegisterInitHook(seedSteererPersona) }
