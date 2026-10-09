package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/promptconduit/cli/internal/cost"
)

// A pre-v0.21 Cursor envelope: input_tokens includes the cache reads, and the
// stored cost priced them twice.
const staleCursorEnvelope = `{"schema":2,"event_id":"e1","tool":"cursor","hook_event":"stop","captured_at":"2026-10-01T12:00:05Z",` +
	`"raw_event":{"hook_event_name":"stop","model":"composer-2.5-fast","conversation_id":"c","generation_id":"g1","session_id":"s","input_tokens":2696092,"output_tokens":4760,"cache_read_tokens":2535648,"cache_write_tokens":0,"workspace_roots":["/p"]},` +
	`"enrichments":{"env":{"os":"darwin"},"cost":{"requests":[{"request_id":"g1","ts":"2026-10-01T12:00:00Z","model":"composer-2.5-fast","model_priced":true,"source":"exact","tokens":{"input":2696092,"output":4760,"cache_read":2535648,"cache_write":0},"usd":{"total":9.43}}],"totals":{"usd":9.43,"currency":"USD","tokens":{"input":2696092,"output":4760,"cache_read":2535648,"cache_write":0}}}}}`

func bundledTable(t *testing.T) *cost.PriceTable {
	t.Helper()
	tbl, err := cost.LoadBundledPriceTable()
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func TestRepriceCursorLine(t *testing.T) {
	tbl := bundledTable(t)
	res := repriceCursorLine([]byte(staleCursorEnvelope), tbl)
	if !res.changed {
		t.Fatal("stale Cursor envelope should be repriced")
	}
	if !(res.after < res.before/2) || res.before != 9.43 {
		t.Fatalf("before %v after %v: want a large drop from 9.43", res.before, res.after)
	}

	var env struct {
		EventID     string `json:"event_id"`
		RawEvent    map[string]any
		Enrichments struct {
			Env  map[string]string `json:"env"`
			Cost struct {
				Requests []struct {
					Timestamp string `json:"ts"`
					Tokens    struct {
						Input int64 `json:"input"`
					} `json:"tokens"`
				} `json:"requests"`
			} `json:"cost"`
		} `json:"enrichments"`
	}
	if err := json.Unmarshal(res.out, &env); err != nil {
		t.Fatal(err)
	}
	if env.EventID != "e1" || env.Enrichments.Env["os"] != "darwin" {
		t.Fatalf("other fields not preserved: %+v", env)
	}
	req := env.Enrichments.Cost.Requests[0]
	if req.Timestamp != "2026-10-01T12:00:00Z" {
		t.Fatalf("ts = %q, want the original request ts kept", req.Timestamp)
	}
	if req.Tokens.Input != 2696092-2535648 {
		t.Fatalf("input = %d, want uncached input", req.Tokens.Input)
	}

	// Idempotent: repricing the repriced line changes nothing.
	if again := repriceCursorLine(res.out, tbl); again.changed {
		t.Fatal("second reprice should be a no-op")
	}
}

func TestRepriceCursorLineIgnoresOthers(t *testing.T) {
	tbl := bundledTable(t)
	for name, line := range map[string]string{
		"claude-code": `{"tool":"claude-code","raw_event":{},"enrichments":{"cost":{"totals":{"usd":1}}}}`,
		"no cost":     `{"tool":"cursor","raw_event":{"hook_event_name":"stop"},"enrichments":{"env":{}}}`,
		"no raw":      `{"tool":"cursor","enrichments":{"cost":{"totals":{"usd":1}}}}`,
		"malformed":   `{"tool":"cursor", "cost": nope`,
	} {
		if res := repriceCursorLine([]byte(line), tbl); res.changed {
			t.Errorf("%s: should be left unchanged", name)
		}
	}
}

// A post-fix event (stored input = raw input minus cache) is never touched,
// even if its stored USD differs from today's rate table.
func TestRepriceCursorLineSkipsPostFixEvents(t *testing.T) {
	line := `{"tool":"cursor","captured_at":"2026-10-09T12:00:00Z",` +
		`"raw_event":{"hook_event_name":"stop","model":"composer-2.5-fast","generation_id":"g","input_tokens":1000,"output_tokens":10,"cache_read_tokens":700,"cache_write_tokens":0},` +
		`"enrichments":{"cost":{"requests":[{"request_id":"g","ts":"2026-10-09T12:00:00Z"}],"totals":{"usd":123.45,"tokens":{"input":300,"output":10,"cache_read":700,"cache_write":0}}}}}`
	if res := repriceCursorLine([]byte(line), bundledTable(t)); res.changed {
		t.Fatal("post-fix event must not be repriced (rate drift is not the bug)")
	}
}

// Preserved payloads keep <, >, & as captured (no < escaping).
func TestRepriceCursorLineKeepsHTMLCharacters(t *testing.T) {
	line := strings.Replace(staleCursorEnvelope, `"workspace_roots":["/p"]`, `"workspace_roots":["/p"],"prompt":"fix <div> && a > b"`, 1)
	res := repriceCursorLine([]byte(line), bundledTable(t))
	if !res.changed {
		t.Fatal("stale envelope should be repriced")
	}
	if !strings.Contains(string(res.out), `"prompt":"fix <div> && a > b"`) {
		t.Fatalf("HTML characters were escaped: %s", res.out)
	}
}

// scanReprice finds stale lines and hands each original to onChange.
func TestScanReprice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	other := `{"tool":"claude-code","enrichments":{}}`
	if err := os.WriteFile(path, []byte(other+"\n"+staleCursorEnvelope+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got []string
	s, err := scanReprice(path, bundledTable(t), func(line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil || s.n != 1 || len(got) != 1 || got[0] != staleCursorEnvelope {
		t.Fatalf("scan = %+v, %v, %d lines; want exactly the stale envelope", s, err, len(got))
	}
}
