// Package webui holds the embedded web UI assets. The embed
// pattern must be relative to this package's directory, so the
// files live in webui/web/. The webapi package reads them
// through FS() and serves them at "/" with the auth
// middleware applied.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed web/*.html web/*.css web/*.js
var assets embed.FS

// FS returns the web UI files as an fs.FS rooted at the
// "web/" subdirectory of this package. The webapi package
// mounts it with http.FileServer so the user only needs to
// remember the listen address.
//
// The root path inside FS is "" (i.e. "web/index.html" is at
// the FS root). The http.FileServer resolves "/" to
// "index.html" automatically.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "web")
	if err != nil {
		// Should never happen — the embed pattern guarantees
		// the "web" prefix exists. If it does, panic: the
		// binary is broken.
		panic("webui: embed sub: " + err.Error())
	}
	return sub
}
