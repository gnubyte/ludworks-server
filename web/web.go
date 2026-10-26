// Package web embeds the browser UI (vanilla ES modules, no build step).
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html all:assets
var files embed.FS

// FS returns the embedded UI tree: index.html at the root and assets/... below it.
func FS() fs.FS { return files }
