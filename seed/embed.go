// Package seedfs embeds the seed state repo of the demo: the final tree of
// main (state/) and the overlays that build the history and the open changes
// (changes/). The format is described in README.md.
package seedfs

import (
	"embed"
	"io/fs"
)

//go:embed all:state all:changes README.md
var files embed.FS

// FS returns the seed tree with state/, changes/ and README.md at its root.
func FS() fs.FS { return files }

// State returns the final tree of main.
func State() fs.FS {
	sub, err := fs.Sub(files, "state")
	if err != nil {
		panic(err)
	}
	return sub
}
