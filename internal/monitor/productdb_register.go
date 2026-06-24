package monitor

// Transitional (plan T13): ensure flowdb.OpenDB creates the connector / github /
// attention product tables this package relies on. productdb is flowdb-free and
// no longer self-registers, so registration lives in productdbreg; this blank
// import triggers it. monitor imports neither server nor steering, so it needs
// its own trigger; productdbreg dedupes, so overlapping triggers are safe.
//
// Removed when every consumer opens the DB via productdb.Open (T13 complete).
import _ "flow/internal/productdbreg"
