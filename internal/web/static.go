// Package web serves the serve-mode web UI: a fixed-sidebar shell with a
// Reports placeholder and a read-only Config view. Templates are pre-generated
// templ (web/templates) and assets are go:embed'd (web/static), so the single
// binary serves the whole UI offline with no runtime template parsing and no
// node dependency.
package web

import (
	"net/http"
	"strings"

	static "github.com/sydlexius/canticle/web/static"
)

// staticPrefix is the URL prefix under which embedded assets are served.
const staticPrefix = "/static/"

// StaticHandler serves the embedded CSS, font, and JS assets under /static/. Two
// caching policies apply:
//   - Fonts (.woff2): content-stable binaries that never change between
//     releases; served with a long-lived immutable header.
//   - CSS (.css) and JS (.js): regenerated/vendored per release under the same
//     filename (no content hash); served with no-cache so clients revalidate on
//     each request. The vendored htmx.min.js is version-pinned but reuses its
//     filename across releases, so it revalidates like CSS rather than caching
//     immutably.
//     TODO: add content-hash suffixes (e.g. output.<hash>.css) to enable true
//     immutable caching for these too.
//
// Misses (404) and HTML pages are never cached.
func StaticHandler() http.Handler {
	fileServer := http.StripPrefix(staticPrefix, http.FileServerFS(static.FS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &cacheOnOK{
			ResponseWriter: w,
			immutable:      strings.HasSuffix(r.URL.Path, ".woff2"),
			revalidate:     strings.HasSuffix(r.URL.Path, ".css") || strings.HasSuffix(r.URL.Path, ".js"),
		}
		fileServer.ServeHTTP(cw, r)
	})
}

// cacheOnOK adds the appropriate Cache-Control header only when the asset is
// found (200), so 404s are never cached. The header must be set before the
// status line is written, which is why it is applied in WriteHeader.
type cacheOnOK struct {
	http.ResponseWriter
	immutable   bool // fonts: content-stable, long-lived immutable header
	revalidate  bool // CSS: same filename per release, revalidate on each request
	wroteHeader bool
}

func (c *cacheOnOK) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	if status == http.StatusOK {
		switch {
		case c.immutable:
			c.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case c.revalidate:
			c.Header().Set("Cache-Control", "no-cache")
		}
	}
	c.ResponseWriter.WriteHeader(status)
}

// Write ensures the Cache-Control header is set before the first byte even when
// the upstream handler writes the body without an explicit WriteHeader call (an
// implicit 200). Without this, a future caller of this wrapper that relies on
// the implicit 200 would bypass the header logic in WriteHeader.
func (c *cacheOnOK) Write(b []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	return c.ResponseWriter.Write(b)
}
