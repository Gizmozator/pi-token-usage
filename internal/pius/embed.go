package pius

import (
	"embed"
	"io/fs"
)

//go:embed all:web
var webAssets embed.FS

// WebFS returns the embedded UI filesystem with the web/ prefix stripped.
func WebFS() fs.FS {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
