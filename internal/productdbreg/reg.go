// Package productdbreg is the transitional glue that registers the flowwyyy
// product migration set (productdb.Ensure) with flowdb's OpenDB.
//
// It exists because internal/productdb is now flowdb-free (Phase-3 ownership
// model, seam §11) and therefore can no longer self-register. Blank-import this
// package wherever flowdb.OpenDB must create the product tables — the product
// binary's wiring (internal/product), and the test binaries that exercise
// product-table code that still lives in flowdb (internal/flowdb,
// internal/briefing). Registration is idempotent per domain
// (flowdb.RegisterMigrations dedupes), so multiple blank-importers in one binary
// are safe.
//
// Import graph: productdbreg → {flowdb, productdb}. productdb does NOT import it
// (so productdb stays flowdb-free), and no CORE binary imports it (so cmd/flow
// stays product-free). Transitional: when every consumer opens the DB via
// productdb.Open (plan T13 complete), registration moves to the flowwyyy main
// and this package is removed.
package productdbreg

import (
	"flow/internal/flowdb"
	"flow/internal/productdb"
)

func init() {
	flowdb.RegisterMigrations(flowdb.MigrationSet{Domain: "flowwyyy", Apply: productdb.Ensure})
}
