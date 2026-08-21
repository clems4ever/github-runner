// Package ui carries the built web interface inside the binary, so installing
// runner-fleet is copying one file.
package ui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the built single-page app, or an error when this binary was
// built without one — which is what a bare "go build" in a checkout that has
// not run the frontend build produces.
func Assets() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, fs.ErrNotExist
	}
	return sub, nil
}
