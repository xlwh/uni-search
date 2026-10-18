// Package deps anchors project-wide third-party dependencies in go.mod.
//
// Several libraries (roaring, compress, gse, protobuf, uniseg) are consumed
// by packages delivered in later milestones. Without a compile-time reference
// `go mod tidy` would strip them from go.mod, forcing future tasks to touch
// module files again. This file keeps the dependency set stable by importing
// each library anonymously.
//
// NOTE: only extend this file when a new task introduces a dependency that
// has no import site yet; remove an entry once real code imports it.
package deps

import (
	_ "github.com/RoaringBitmap/roaring"
	_ "github.com/go-ego/gse"
	_ "github.com/klauspost/compress"
	_ "github.com/rivo/uniseg"
	_ "google.golang.org/protobuf/proto"
)
