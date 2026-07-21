package webapp

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// MountPath is the prefix under which the React dashboard is
// served. Phase 8 cutover (2026-05-16): collapsed from /v2/ to
// /. Vite is configured with the matching `base: "/"`.
const MountPath = "/"

// IndexHTML returns the raw bytes of the embedded build's index.html, or nil
// when it is absent (a broken embed). The dashboard's execute-tier CSP reads
// this to compute the SHA-256 hash of any INLINE <script> the Vite build emits
// (the theme pre-paint), so `script-src` can allow exactly those inline blocks
// without an 'unsafe-inline' fallback — and the hash stays in lock-step with the
// actual shipped HTML instead of drifting from a hard-coded constant.
func IndexHTML() []byte {
	b, err := distFS.ReadFile("dist/index.html")
	if err != nil {
		return nil
	}
	return b
}

// Handler returns an http.Handler that serves the embedded React
// app at root. Unknown paths fall back to index.html so React
// Router can render client-side routes.
//
// Asset requests under /assets/* (the fingerprinted Vite output)
// resolve via the FileServer; everything else is a client-side
// route and gets the SPA shell.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Static embed root is fixed; a failure here is a build-time bug.
		panic("webapp: dist/ embed sub failed: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			serveIndex(w, r, sub)
			return
		}
		// Direct hit on an asset (incl. /assets/* fingerprinted bundles).
		if f, err := sub.Open(clean); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// Anything else is a client-side route — fall back to index.html.
		serveIndex(w, r, sub)
	})
}

func serveIndex(w http.ResponseWriter, _ *http.Request, sub fs.FS) {
	f, err := sub.Open("index.html")
	if err != nil {
		http.Error(w, "webapp: index.html missing", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.Copy(w, f)
}
