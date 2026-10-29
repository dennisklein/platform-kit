// Package web embeds the built web UI (web/dist, made by npm run build).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built UI. It holds only .gitkeep until the UI is built;
// the bench then serves a fallback page.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
