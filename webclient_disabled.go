//go:build !webclient

package haunt

import "io/fs"

// Source-only API builds do not need Node. Release builds use -tags webclient.
func WebClient() fs.FS { return nil }
