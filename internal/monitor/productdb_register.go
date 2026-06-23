package monitor

// Transitional: pull in the product DB schema so flowdb.OpenDB creates the
// connector/github/attention tables this package relies on. monitor imports
// neither server nor steering, so it needs its own registration. The blank
// import runs productdb's init(), which registers the product migration set.
//
// productdb does not import monitor, so there is no cycle. When the two-binary
// split lands (plan T9–T10), cmd/flowwyyy / internal/product owns this
// registration and this file should be removed.
import _ "flow/internal/productdb"
