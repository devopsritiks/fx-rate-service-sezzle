// Package webui embeds and serves the single-page demo UI from
// internal/webui/static. There is no build step and no Node/npm
// dependency: the page is plain HTML/CSS/JS, embedded straight into the
// fxservice binary with go:embed so it ships as part of the same
// container image and is served from the same origin as the API —
// no separate UI container, no CORS configuration needed, since the
// page's own fetch() calls to /v1/... are same-origin.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var staticFiles embed.FS

// Handler serves the demo UI at the path it's mounted on (expected: "/").
func Handler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// Only possible if the embed path above is wrong, which would be
		// a build-time mistake, not a runtime condition — panicking here
		// (at server-construction time, not per-request) surfaces that
		// immediately instead of silently serving 404s for the UI.
		panic("webui: static assets not embedded correctly: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
