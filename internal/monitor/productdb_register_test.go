package monitor

// Test-only: blank-import productdbreg so flowdb.OpenDB creates the
// connector/github/attention product tables in monitor's TEST binary (its tests
// seed/read those tables directly). In production the server/product side owns
// this registration when it opens the DB — monitor's NON-test code stays
// flowdb-free, which is what removes monitor from the T13 import-guard ratchet
// (Phase-3 ownership model, seam §11). productdbreg dedupes, so overlapping
// triggers are safe.
import _ "flow/internal/productdbreg"
