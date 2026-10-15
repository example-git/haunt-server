//go:build webclient

// Package haunt provides the web assets embedded in release server binaries.
package haunt

import (
	"embed"
	"io/fs"
)

//go:embed all:client/dist
var builtClient embed.FS

func WebClient() fs.FS {
	files, err := fs.Sub(builtClient, "client/dist")
	if err != nil {
		panic(err)
	}
	return files
}
