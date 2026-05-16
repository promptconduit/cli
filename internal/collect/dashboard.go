package collect

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
)

//go:embed ui/index.html
var dashboardHTML []byte

// mountDashboard registers the dashboard routes onto mux.
//
//	GET /              → embedded HTML
//	GET /api/traces    → JSON list of recent traces (?limit=N)
//	GET /api/spans     → JSON list of recent spans (?trace_id=..&limit=N)
func mountDashboard(mux *http.ServeMux, store *Store) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})

	mux.HandleFunc("/api/traces", func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 50)
		traces, err := store.ListTraces(limit)
		writeJSON(w, traces, err)
	})

	mux.HandleFunc("/api/spans", func(w http.ResponseWriter, r *http.Request) {
		limit := intParam(r, "limit", 500)
		traceID := r.URL.Query().Get("trace_id")
		spans, err := store.ReadSpans(limit, traceID)
		writeJSON(w, spans, err)
	})
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, body any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	switch v := body.(type) {
	case []SpanRow:
		if v == nil {
			_, _ = w.Write([]byte("[]\n"))
			return
		}
	case []TraceSummary:
		if v == nil {
			_, _ = w.Write([]byte("[]\n"))
			return
		}
	}
	_ = json.NewEncoder(w).Encode(body)
}
