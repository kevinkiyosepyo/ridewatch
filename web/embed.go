// Package web contains the rider-facing frontend, embedded into the binary.
//
// Vendored assets: MapLibre GL JS v4.7.1 (maplibre-gl.js + maplibre-gl.css),
// resolved from https://unpkg.com/maplibre-gl@4/dist/ and committed so nothing
// is fetched from a CDN at runtime.
package web

import "embed"

// FS is the frontend asset tree; cmd passes it into api.New.
//
//go:embed index.html stop.html app.js stop.js style.css sw.js vendor
var FS embed.FS
