package pius

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server exposes the index over HTTP and serves the embedded UI.
type Server struct {
	store       *Store
	root        string
	openCodeDB  string
	ocrRoot     string
	webFS       fs.FS
	mu          sync.Mutex
	indexing    bool
	lastResult  *IndexResult
	lastRefresh time.Time
	refreshErr  string
}

// NewServer builds the HTTP server. openCodeDB and ocrRoot may be "" to
// exclude those sources.
func NewServer(s *Store, sessionsRoot, openCodeDB, ocrRoot string, web fs.FS) *Server {
	return &Server{store: s, root: sessionsRoot, openCodeDB: openCodeDB, ocrRoot: ocrRoot, webFS: web}
}

// Handler returns the routed mux.
func (srv *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/bootstrap", srv.handleBootstrap)
	mux.HandleFunc("GET /api/totals", srv.handleTotals)
	mux.HandleFunc("GET /api/cube", srv.handleCube)
	mux.HandleFunc("GET /api/sessions", srv.handleSessions)
	mux.HandleFunc("GET /api/status", srv.handleStatus)
	mux.HandleFunc("POST /api/index", srv.handleIndex)

	if srv.webFS != nil {
		fileServer := http.FileServer(http.FS(srv.webFS))
		mux.Handle("GET /", noCache(fileServer))
	}
	return logMiddleware(gzipMiddleware(mux))
}

// Bootstrap is the single payload the UI needs. Everything after this is
// client-side filtering: the cube is a couple of KB, so round-tripping per filter
// change would cost more than the data itself.
type Bootstrap struct {
	GeneratedAt string       `json:"generated_at"`
	Index       IndexInfo    `json:"index"`
	Totals      Totals       `json:"totals"`
	Cube        []CubeRow    `json:"cube"`
	Sessions    []SessionRow `json:"sessions"`
	Facets      Facets       `json:"facets"`
}

// IndexInfo describes index freshness.
type IndexInfo struct {
	LastIndex   string `json:"last_index"`
	Refreshed   bool   `json:"refreshed"`
	RefreshMS   int64  `json:"refresh_ms"`
	FilesTotal  int64  `json:"files_total"`
	EventsTotal int64  `json:"events_total"`
}

// Facets are the distinct filter values, so the UI can build selects without
// scanning the cube itself.
type Facets struct {
	Projects  []string `json:"projects"`
	Models    []string `json:"models"`
	Providers []string `json:"providers"`
	Kinds     []string `json:"kinds"`
	Sources   []string `json:"sources"`
	Days      []string `json:"days"`
}

func (srv *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	refreshed, ms, err := srv.maybeRefresh(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}

	tot, err := srv.store.Totals()
	if err != nil {
		httpError(w, err)
		return
	}
	cube, err := srv.store.Cube()
	if err != nil {
		httpError(w, err)
		return
	}
	sess, err := srv.store.Sessions(400)
	if err != nil {
		httpError(w, err)
		return
	}
	stats, err := srv.store.Stats()
	if err != nil {
		httpError(w, err)
		return
	}

	bs := Bootstrap{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Index: IndexInfo{
			LastIndex:   srv.store.GetMeta("last_index"),
			Refreshed:   refreshed,
			RefreshMS:   ms,
			FilesTotal:  stats["files"],
			EventsTotal: stats["usage_events"],
		},
		Totals:   tot,
		Cube:     cube,
		Sessions: sess,
		Facets:   facetsFrom(cube),
	}
	writeJSON(w, r, bs)
}

// maybeRefresh does the cheap staleness check and indexes only changed files.
// Called on page load rather than from a background watcher: the stat pass costs a
// few milliseconds, which is strictly better than any polling interval.
func (srv *Server) maybeRefresh(ctx interface{ Done() <-chan struct{} }) (bool, int64, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()

	// Coalesce: if we refreshed very recently, don't re-stat again.
	if time.Since(srv.lastRefresh) < 1500*time.Millisecond {
		return false, 0, nil
	}
	if srv.indexing {
		return false, 0, nil
	}
	srv.indexing = true
	defer func() { srv.indexing = false }()

	start := time.Now()
	res, changed, err := EnsureFresh(srv.store, srv.root, srv.openCodeDB, srv.ocrRoot)
	srv.lastRefresh = time.Now()
	if err != nil {
		srv.refreshErr = err.Error()
		return false, 0, err
	}
	srv.refreshErr = ""
	srv.lastResult = res
	return changed, time.Since(start).Milliseconds(), nil
}

func (srv *Server) handleTotals(w http.ResponseWriter, r *http.Request) {
	t, err := srv.store.Totals()
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, r, t)
}

func (srv *Server) handleCube(w http.ResponseWriter, r *http.Request) {
	c, err := srv.store.Cube()
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, r, c)
}

func (srv *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	s, err := srv.store.Sessions(limit)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, r, s)
}

func (srv *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := srv.store.Stats()
	if err != nil {
		httpError(w, err)
		return
	}
	srv.mu.Lock()
	idx := srv.indexing
	errS := srv.refreshErr
	last := srv.lastRefresh
	var lastRes *IndexResult
	if srv.lastResult != nil {
		cp := *srv.lastResult
		lastRes = &cp
	}
	srv.mu.Unlock()

	writeJSON(w, r, map[string]any{
		"indexing":      idx,
		"last_index":    srv.store.GetMeta("last_index"),
		"last_refresh":  last.UTC().Format(time.RFC3339),
		"refresh_error": errS,
		"last_result":   lastRes,
		"tables":        stats,
		"sessions_root": srv.root,
		"opencode_db":   srv.openCodeDB,
		"ocr_root":      srv.ocrRoot,
	})
}

func (srv *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	full := r.URL.Query().Get("full") == "true"

	srv.mu.Lock()
	if srv.indexing {
		srv.mu.Unlock()
		http.Error(w, "index already running", http.StatusConflict)
		return
	}
	srv.indexing = true
	srv.mu.Unlock()

	start := time.Now()
	res, err := Index(srv.store, IndexOptions{Root: srv.root, OpenCodeDB: srv.openCodeDB, OCRRoot: srv.ocrRoot, Full: full})
	srv.mu.Lock()
	srv.indexing = false
	srv.lastRefresh = time.Now()
	if err == nil {
		srv.lastResult = res
	} else {
		srv.refreshErr = err.Error()
	}
	srv.mu.Unlock()

	if err != nil {
		httpError(w, err)
		return
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	writeJSON(w, r, res)
}

func facetsFrom(cube []CubeRow) Facets {
	f := Facets{Projects: []string{}, Models: []string{}, Providers: []string{}, Kinds: []string{}, Sources: []string{}, Days: []string{}}
	seenP := map[string]bool{}
	seenM := map[string]bool{}
	seenV := map[string]bool{}
	seenK := map[string]bool{}
	seenS := map[string]bool{}
	seenD := map[string]bool{}
	for _, c := range cube {
		if c.Project != "" && !seenP[c.Project] {
			seenP[c.Project] = true
			f.Projects = append(f.Projects, c.Project)
		}
		if c.Model != "" && !seenM[c.Model] {
			seenM[c.Model] = true
			f.Models = append(f.Models, c.Model)
		}
		if c.Provider != "" && !seenV[c.Provider] {
			seenV[c.Provider] = true
			f.Providers = append(f.Providers, c.Provider)
		}
		if c.Kind != "" && !seenK[c.Kind] {
			seenK[c.Kind] = true
			f.Kinds = append(f.Kinds, c.Kind)
		}
		if c.Source != "" && !seenS[c.Source] {
			seenS[c.Source] = true
			f.Sources = append(f.Sources, c.Source)
		}
		if c.Day != "" && !seenD[c.Day] {
			seenD[c.Day] = true
			f.Days = append(f.Days, c.Day)
		}
	}
	sortStrings(f.Projects)
	sortStrings(f.Models)
	sortStrings(f.Providers)
	sortStrings(f.Kinds)
	sortStrings(f.Sources)
	sortStrings(f.Days)
	return f
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	// Compact encoding: pretty-printing roughly doubles the payload, and the cube
	// is the one thing the browser always downloads in full. Compression is
	// handled by gzipMiddleware so it is applied exactly once.
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf)
}

// gzipResponseWriter compresses transparently for handlers we do not control
// (the static file server), while keeping Content-Length out of the way.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	// Content-Encoding is set by the middleware before dispatch, because a handler
	// that only calls Write() never reaches this override - setting it here would
	// silently ship gzip bytes with no header and break JSON parsing client-side.
	g.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	return g.gz.Write(p)
}

func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		// Declare the encoding up front: by the time any handler writes, the header
		// must already be present.
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
		_ = gz.Close()
	})
}

func httpError(w http.ResponseWriter, err error) {
	log.Printf("api error: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if len(r.URL.Path) >= 4 && r.URL.Path[:5] == "/api/" {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// Healthcheck dries the listener and returns nil if it answers.
func Healthcheck(addr string, timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	_ = c.Close()
	return nil
}

var _ = fmt.Sprintf
