package product

import "flow/internal/cli"

func registerCommands() {
	cli.Register(cli.Command{Name: "ui", Run: cmdUI, Help: "  flow ui serve [--host] [--port] [--bg]"})
	cli.Register(cli.Command{Name: "serve", Run: cmdUIServe, Hidden: true})
	cli.Register(cli.Command{Name: "attention", Run: cmdAttention, Help: "  flow attention list|act|trace|feedback ..."})
	cli.Register(cli.Command{Name: "slack", Run: cmdSlack, Help: "  flow slack send|react ..."})
}

func init() { registerCommands() }
