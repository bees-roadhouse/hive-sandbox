// Package webui embeds the frontend assets. It exists because //go:embed can
// only see files beneath its own directory, and this module's root is where
// the frontend lives relative to that.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend
var files embed.FS

// FS returns the frontend rooted at its own directory.
func FS() fs.FS {
	sub, err := fs.Sub(files, "frontend")
	if err != nil {
		// An embed pattern that matched nothing fails at compile time; this
		// path is unreachable and says so rather than pretending.
		panic("desktop: embedded frontend missing: " + err.Error())
	}
	return sub
}
