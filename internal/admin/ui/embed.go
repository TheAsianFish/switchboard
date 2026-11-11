// Package ui holds the built admin page (web/, built by `make ui`) so the
// binary serves it without Node. The build output is committed: `go
// install` and `go build` need no frontend toolchain, and `make ui-check`
// fails when it no longer matches the sources.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS is the built page, rooted at its index.html.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}
