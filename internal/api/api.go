// Package api serves the RideWatch HTTP surface: the JSON API under /api,
// health and metrics endpoints, and the embedded static frontend.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kevinkiyosepyo/ridewatch/internal/config"
	"github.com/kevinkiyosepyo/ridewatch/internal/domain"
)

// upcomingHorizon is how far ahead /api/stops/{id}/upcoming looks.
const upcomingHorizon = 90 * time.Minute

const (
	searchLimit = 25
	bboxLimit   = 500
)

type server struct {
	cfg    config.Config
	q      domain.StopQueries
	live   domain.LiveSource
	subs   domain.SubscriptionStore
	assets map[string]staticAsset
	log    *slog.Logger
}

// staticAsset is one embedded file, read and hashed once at startup. The
// frontend is baked into the binary, so its bytes cannot change while the
// process runs: holding them lets every request skip a read, and lets the
// ETag be a plain content hash.
type staticAsset struct {
	data  []byte
	etag  string
	ctype string
}

// loadAssets walks the embedded tree once. A file that cannot be read is
// skipped rather than fatal: a missing asset should 404, not refuse to boot.
func loadAssets(fsys fs.FS, log *slog.Logger) map[string]staticAsset {
	assets := make(map[string]staticAsset)
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := fs.ReadFile(fsys, name)
		if rerr != nil {
			log.Warn("static asset unreadable, skipping", "name", name, "err", rerr)
			return nil
		}
		sum := sha256.Sum256(data)
		assets[name] = staticAsset{
			data:  data,
			etag:  `"` + hex.EncodeToString(sum[:16]) + `"`,
			ctype: contentTypeFor(name),
		}
		return nil
	})
	if err != nil {
		log.Warn("walking static assets", "err", err)
	}
	return assets
}

// New builds the complete HTTP handler: JSON API, healthz, Prometheus metrics,
// and the static frontend (SPA fallback for extensionless paths).
func New(cfg config.Config, q domain.StopQueries, live domain.LiveSource,
	subs domain.SubscriptionStore, static fs.FS) http.Handler {
	log := slog.Default()
	s := &server{cfg: cfg, q: q, live: live, subs: subs,
		assets: loadAssets(static, log), log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/vehicles", s.handleVehicles)
	mux.HandleFunc("GET /api/stops", s.handleStops)
	mux.HandleFunc("GET /api/stops/{id}", s.handleStop)
	mux.HandleFunc("GET /api/stops/{id}/upcoming", s.handleUpcoming)
	mux.HandleFunc("GET /api/stops/{id}/reliability", s.handleStopReliability)
	mux.HandleFunc("GET /api/routes", s.handleRoutes)
	mux.HandleFunc("GET /api/routes/{id}/reliability", s.handleRouteReliability)
	mux.HandleFunc("GET /api/feedinfo", s.handleFeedInfo)
	mux.HandleFunc("GET /api/vapid-public-key", s.handleVAPIDPublicKey)
	mux.HandleFunc("POST /api/subscriptions", s.handleSubscribe)
	mux.HandleFunc("DELETE /api/subscriptions", s.handleUnsubscribe)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /stop/{id}", s.handleStopPage)
	mux.HandleFunc("/", s.handleStatic)

	return s.logging(s.recovering(mux))
}

// --- middleware ---

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (s *server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		s.log.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func (s *server) recovering(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			s.log.Error("panic serving request",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
			writeError(w, http.StatusInternalServerError, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Warn("write json response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// --- static frontend ---

var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".webmanifest": "application/manifest+json",
	".woff2":       "font/woff2",
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// serveStaticFile writes one embedded asset with a content ETag. Filenames are
// unhashed (app.js, not app.9f3c.js), so the assets must never be cached
// blind: no-cache means "revalidate every time", and the ETag makes that
// revalidation a 304 instead of a re-download — the difference between a few
// hundred bytes and the 800 KB MapLibre bundle on a repeat visit.
//
// http.ServeContent handles If-None-Match, HEAD, and range requests; the zero
// modTime keeps it from inventing a Last-Modified, since embedded files have
// no meaningful timestamp.
func (s *server) serveStaticFile(w http.ResponseWriter, r *http.Request, name string) {
	a, ok := s.assets[name]
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", a.ctype)
	w.Header().Set("ETag", a.etag)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.data))
}

// handleStopPage serves the stop detail page; the id in the URL is read by
// client-side JS, the server just returns the shell.
func (s *server) handleStopPage(w http.ResponseWriter, r *http.Request) {
	s.serveStaticFile(w, r, "stop.html")
}

// handleStatic serves embedded assets. Unknown extensionless paths fall back to
// index.html (SPA routing); unknown asset paths 404.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if _, ok := s.assets[name]; !ok {
		if path.Ext(name) == "" {
			s.serveStaticFile(w, r, "index.html")
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.serveStaticFile(w, r, name)
}
