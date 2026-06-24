package steering

// Transitional (plan T13): ensure flowdb.OpenDB creates the attention / steering
// / connector product tables this package relies on. productdb is flowdb-free
// and no longer self-registers, so registration lives in productdbreg; this
// blank import triggers it. steering is on the common import path for both
// DB-opening product entrypoints (server→steering and attention→steering), so
// this covers them; productdbreg dedupes, so overlapping triggers are safe.
//
// Removed when every consumer opens the DB via productdb.Open (T13 complete).
import _ "flow/internal/productdbreg"
