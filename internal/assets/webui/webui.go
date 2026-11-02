// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

// Package webui provides the built Svelte frontend. It is embedded so the
// desktop and server binaries stay single files. Before the first frontend
// build the directory holds only a placeholder, so the Go build still
// compiles; the server then reports that the dashboard is not embedded.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// StaticFS returns the built frontend filesystem.
func StaticFS() fs.FS {
	sub, _ := fs.Sub(dist, "dist")
	return sub
}

// HasBuild reports whether a frontend build is present. Without one the
// server compiles and serves the API but has no dashboard to serve.
func HasBuild() bool {
	_, err := dist.ReadFile("dist/index.html")
	return err == nil
}
