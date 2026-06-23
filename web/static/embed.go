// Package static embeds the serve-mode web UI's static assets (compiled CSS and
// self-hosted fonts) into the binary at build time, so the single binary serves
// the UI offline without needing the web/static directory on disk.
//
// The embed directive cannot reach up out of its own directory tree, so it lives
// here inside web/static rather than in internal/web (mirrors stillwater's
// web/static/embed.go). internal/web wraps FS into the HTTP handler.
package static

import "embed"

// FS holds every static asset under web/static except Go sources. The css
// directory carries the Tailwind output (output.css) plus its sources; fonts
// carries the woff2 files and their OFL license texts; js carries the vendored
// htmx runtime (htmx.min.js, 0BSD) and Chart.js (chart.umd.min.js, MIT, pinned
// v4.4.9) plus the dashboard chart init (chart-init.js) and their license texts.
//
//go:embed all:css all:fonts all:img all:js
var FS embed.FS
