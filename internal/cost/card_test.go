package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedCardOverridesNewerRowsOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	writeCard(t, `{
      "_source": "promptconduit",
      "_updated": "2026-09-26",
      "claude-opus-4-8": {"input_cost_per_token": 0.000009, "output_cost_per_token": 0.000009},
      "brand-new-model": {"input_cost_per_token": 0.000003, "output_cost_per_token": 0.000004}
    }`)

	tbl, err := LoadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	opus, ok := tbl.ResolvePrice("claude-opus-4-8")
	if !ok || opus.Input != 0.000009 {
		t.Fatalf("newer card should override embed; got %v ok=%v", opus.Input, ok)
	}
	fresh, ok := tbl.ResolvePrice("brand-new-model")
	if !ok || fresh.Input != 0.000003 {
		t.Fatalf("card-only model should resolve; got %v ok=%v", fresh.Input, ok)
	}
}

func TestOlderOrUnmarkedCardDoesNotOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	writeCard(t, `{
      "_source": "promptconduit",
      "_updated": "2020-01-01",
      "claude-opus-4-8": {"input_cost_per_token": 0.000009, "output_cost_per_token": 0.000009}
    }`)
	tbl, err := LoadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	opus, ok := tbl.ResolvePrice("claude-opus-4-8")
	if !ok || opus.Input == 0.000009 {
		t.Fatalf("older card must not override embed; got %v ok=%v", opus.Input, ok)
	}

	writeCard(t, `{
      "_updated": "2026-09-26",
      "claude-opus-4-8": {"input_cost_per_token": 0.000009, "output_cost_per_token": 0.000009}
    }`)
	tbl, err = LoadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	opus, ok = tbl.ResolvePrice("claude-opus-4-8")
	if !ok || opus.Input == 0.000009 {
		t.Fatalf("card without _source must not override; got %v ok=%v", opus.Input, ok)
	}
}

func TestRefreshPublishedCardRejectsBadPayload(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"_source":"promptconduit","_updated":"2026-09-26","only-one":{"input_cost_per_token":1,"output_cost_per_token":1}}`))
	}))
	defer srv.Close()

	if _, err := RefreshPublishedCard(context.Background(), srv.URL); err == nil {
		t.Fatal("short card should be rejected")
	}
	if _, err := os.Stat(PublishedCardPath()); !os.IsNotExist(err) {
		t.Fatal("rejected download must not write a card")
	}
}

func TestRefreshPublishedCardWrites(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	body := map[string]any{
		"_source":  "promptconduit",
		"_updated": "2026-09-26",
	}
	for i := 0; i < 10; i++ {
		body[fmt.Sprintf("model-%d", i)] = map[string]float64{
			"input_cost_per_token":  0.000001,
			"output_cost_per_token": 0.000002,
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	n, err := RefreshPublishedCard(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("priced models = %d, want 10", n)
	}
	tbl, err := LoadPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tbl.ResolvePrice("model-0")
	if !ok || got.Input != 0.000001 {
		t.Fatalf("downloaded card did not load; got %v ok=%v", got.Input, ok)
	}
}

func writeCard(t *testing.T, body string) {
	t.Helper()
	path := PublishedCardPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
