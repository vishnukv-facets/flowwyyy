package product

import "flow/internal/cli"

func registerCommands() {
	cli.Register(cli.Command{Name: "ui", Run: cmdUI, Help: "  flow ui serve [--host] [--port] [--bg]"})
	cli.Register(cli.Command{Name: "serve", Run: cmdUIServe, Hidden: true})
	cli.Register(cli.Command{Name: "attention", Run: cmdAttention, Help: "  flow attention list|act|trace|feedback ..."})
	cli.Register(cli.Command{Name: "slack", Run: cmdSlack, Help: "  flow slack send|react ..."})
	// Override the core skill handler so the flowwyyy binary installs the full
	// composed (core + product) skill rather than passing through to the
	// core-only installer. Runs after app.init's registerCore (product imports
	// app), so this registration wins in the shared registry.
	cli.Register(cli.Command{Name: "skill", Run: cmdSkill, Help: "  flow skill install|update|print|uninstall"})
}

func init() { registerCommands() }
