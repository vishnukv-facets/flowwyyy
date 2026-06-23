package steering

// Transitional: pull in the product DB schema so flowdb.OpenDB creates the
// attention/steering/connector tables this package relies on. The blank import
// runs productdb's init(), which registers the product migration set.
//
// This package imports flowdb (core) and productdb (product); productdb does
// not import steering, so there is no cycle. When the two-binary split lands
// (plan T9–T10), cmd/flowwyyy / internal/product owns this registration and
// this file should be removed.
import _ "flow/internal/productdb"
