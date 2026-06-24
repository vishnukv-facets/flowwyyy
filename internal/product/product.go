package product

import (
	// Registers the flowwyyy product migration set on the shared DB so the
	// product tables (attention_*, steering_*, github_*, chats, …) are created/
	// migrated when the flowwyyy binary opens flow.db. productdb is flowdb-free
	// and no longer self-registers (seam §11), so registration lives in
	// productdbreg. (Also pulled transitively via server→monitor/steering;
	// productdbreg's registration is idempotent per domain.)
	_ "flow/internal/productdbreg"
)

// FlowBin is the resolved core `flow` binary, set by RunWithPassthrough in the
// flowwyyy main. Product commands (ui serve) use it for the server's flowclient
// (mutations + launch prep). Empty when the product package is used outside the
// flowwyyy main (tests), where cmdUIServe falls back to the running executable.
var FlowBin string
