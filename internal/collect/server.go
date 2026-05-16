// Package collect implements a local-first OTLP/HTTP receiver with an
// embedded NDJSON store and a read-only dashboard. It's the open-source
// half of PromptConduit's agent-tracing stack: it works standalone, with
// no platform backend required, and is intended for both dogfooding and
// for users who want a self-hostable view of their agent traces.
package collect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/promptconduit/cli/internal/client"
)

// Options configures a Collector.
type Options struct {
	// OTLPAddr is the listen address for the OTLP/HTTP receiver
	// (e.g. "127.0.0.1:4318"). Required.
	OTLPAddr string

	// DashboardAddr is the listen address for the dashboard (HTML + JSON
	// API). Required.
	DashboardAddr string

	// StoreDir is where NDJSON span/log files are written. Empty means
	// ~/.config/promptconduit/collect/.
	StoreDir string
}

// Collector ties the OTLP receiver, store, and dashboard together.
type Collector struct {
	store   *Store
	otlpSrv *http.Server
	dashSrv *http.Server
}

// New builds a Collector. Returns an error if the store can't be opened.
func New(opts Options) (*Collector, error) {
	if opts.OTLPAddr == "" {
		return nil, errors.New("OTLPAddr is required")
	}
	if opts.DashboardAddr == "" {
		return nil, errors.New("DashboardAddr is required")
	}

	dir := opts.StoreDir
	if dir == "" {
		dir = filepath.Join(client.ConfigDir(), "collect")
	}

	store, err := OpenStore(dir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	otlpMux := http.NewServeMux()
	otlpMux.HandleFunc("/v1/traces", newOTLPHandler(store, signalTraces))
	otlpMux.HandleFunc("/v1/logs", newOTLPHandler(store, signalLogs))
	// Accept-and-discard metrics for now so emitters that ship all three
	// signals don't error out — we just don't expose them in the UI yet.
	otlpMux.HandleFunc("/v1/metrics", okHandler)
	otlpMux.HandleFunc("/", notFoundHandler)

	dashMux := http.NewServeMux()
	mountDashboard(dashMux, store)

	return &Collector{
		store: store,
		otlpSrv: &http.Server{
			Addr:              opts.OTLPAddr,
			Handler:           otlpMux,
			ReadHeaderTimeout: 5 * time.Second,
		},
		dashSrv: &http.Server{
			Addr:              opts.DashboardAddr,
			Handler:           dashMux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}, nil
}

// StoreDir returns the directory the collector writes to.
func (c *Collector) StoreDir() string { return c.store.Dir() }

// Run starts the OTLP receiver and dashboard, blocking until ctx is
// cancelled or one of them fails.
func (c *Collector) Run(ctx context.Context) error {
	errCh := make(chan error, 2)
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := c.otlpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("otlp: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := c.dashSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("dashboard: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		shutdown(c.otlpSrv, c.dashSrv)
		wg.Wait()
		_ = c.store.Close()
		return err
	}

	shutdown(c.otlpSrv, c.dashSrv)
	wg.Wait()
	return c.store.Close()
}

func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx)
	}
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{}`))
}

func notFoundHandler(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "not found", http.StatusNotFound)
}
