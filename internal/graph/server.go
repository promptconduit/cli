package graph

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

//go:embed ui/graph.html
var graphHTML []byte

// Options configures a Server.
type Options struct {
	// Addr is the listen address (e.g. "127.0.0.1:4320"). Required.
	Addr string
	// EventsPath overrides the events.jsonl path; empty uses the default.
	EventsPath string
	// Backfill is how many recent lines to seed before going live.
	Backfill int
}

// Server serves the live Session Graph page plus the /api/events poll endpoint,
// backed by a Hub tailing events.jsonl.
type Server struct {
	http *http.Server
	hub  *Hub
}

// New builds a Server and starts the tail (bound to ctx).
func New(ctx context.Context, opts Options) (*Server, error) {
	if opts.Addr == "" {
		return nil, errors.New("Addr is required")
	}
	hub := NewHub(ctx, opts.EventsPath, opts.Backfill)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(graphHTML)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		after := intParam(r, "after", -1)
		limit := intParam(r, "limit", 5000)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(hub.Since(after, limit))
	})

	return &Server{
		http: &http.Server{Addr: opts.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		hub:  hub,
	}, nil
}

// Run serves until ctx is cancelled or the listener fails.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		s.shutdown()
		return err
	}
	s.shutdown()
	return nil
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
}

// intParam parses ?name=N, returning def when absent or unparseable. Unlike the
// collect dashboard's helper this accepts non-positive values (after=0/-1 are
// meaningful cursors here).
func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
