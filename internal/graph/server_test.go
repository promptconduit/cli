package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer builds a Server's mux directly with a pre-seeded hub, avoiding a
// real listener and the file tailer.
func testHandler(h *Hub) http.Handler {
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
		_ = json.NewEncoder(w).Encode(h.Since(after, limit))
	})
	return mux
}

func TestServesEmbeddedPage(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(&Hub{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	// The embedded bundle must actually be present and be the graph page.
	if body := rec.Body.String(); !strings.Contains(body, "PromptConduit") || len(body) < 1000 {
		t.Fatalf("embedded page looks wrong: %d bytes", len(body))
	}
}

func TestEventsEndpointReturnsSeededLines(t *testing.T) {
	h := newHubWith(`{"schema":2,"hook_event":"UserPromptSubmit"}`, `{"schema":2,"hook_event":"Stop"}`)
	rec := httptest.NewRecorder()
	testHandler(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events?after=-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var b Batch
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(b.Lines) != 2 || b.Cursor != 1 {
		t.Fatalf("batch = %+v, want 2 lines cursor 1", b)
	}
}

func TestEventsEndpointIncremental(t *testing.T) {
	h := newHubWith("a", "b")
	rec := httptest.NewRecorder()
	testHandler(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events?after=1", nil))
	var b Batch
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if len(b.Lines) != 0 || b.Cursor != 1 {
		t.Fatalf("batch = %+v, want no new lines cursor 1", b)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(&Hub{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Smoke test that New wires a hub + server without a real events.jsonl.
func TestNewRequiresAddr(t *testing.T) {
	if _, err := New(context.Background(), Options{}); err == nil {
		t.Fatal("New with empty Addr should error")
	}
}
