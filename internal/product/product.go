package product

// product no longer blank-imports productdbreg: the flowwyyy runtime opens the
// DB via productdb.Open (in cmdUIServe / openAttentionDB), which creates the
// Bucket-F product tables directly — so the transitional flowdb.OpenDB
// registration shim is no longer on the product binary's path. This severs
// product's last transitive flowdb import (seam §11, T13). Test binaries that
// still open via flowdb.OpenDB blank-import productdbreg themselves.

// FlowBin is the resolved core `flow` binary, set by RunWithPassthrough in the
// flowwyyy main. Product commands (ui serve) use it for the server's flowclient
// (mutations + launch prep). Empty when the product package is used outside the
// flowwyyy main (tests), where cmdUIServe falls back to the running executable.
var FlowBin string

// Version is the flowwyyy build version, stamped by the flowwyyy main
// (-ldflags -X main.version). Mission Control's version display reads it. It is
// the product binary's OWN version, independent of the core `flow` binary's
// (which flowwyyy execs and version-checks separately) — so it lives here rather
// than reaching into internal/app (Phase-3 decoupling, seam §11.3.1, Tier B).
var Version = "dev"
