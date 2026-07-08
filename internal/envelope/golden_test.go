package envelope

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testdata/envelope.golden.json is the CANONICAL, fully-populated v2 envelope —
// the single cross-repo contract sample. It is the source of truth mirrored
// (byte-for-byte, additive-only) into:
//
//	platform/app/api/src/test/envelope.golden.json      (validated in envelope.golden.test.ts)
//	editor-extension/src/test/envelope.golden.json      (parsed in envelope.golden.test.ts)
//
// When you change the envelope wire shape, update this file AND both mirrors in
// the same coordinated change. These three tests are the guard: they fail if a
// repo's view can no longer carry the canonical envelope, catching the drift the
// hand-maintained mirrors would otherwise accumulate silently.

// wellKnownSlugs are the enrichment slugs every reader is expected to understand.
// Keep in sync with cli/CLAUDE.md, platform types/envelope.ts EnvelopeEnrichments,
// and editor-extension src/envelope.ts.
var wellKnownSlugs = []string{
	"env", "trace", "vcs", "prompt", "cost", "tools", "diff", "subagent", "turn", "permission",
}

func loadGolden(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "envelope.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return b
}

// TestGoldenEnvelope_RoundTrips asserts the Go struct can faithfully carry the
// canonical envelope: every top-level field and every well-known enrichment slug
// survives unmarshal → struct → marshal. A renamed/removed json tag on
// RawEventEnvelope drops the field here and fails the test.
func TestGoldenEnvelope_RoundTrips(t *testing.T) {
	golden := loadGolden(t)

	var env RawEventEnvelope
	if err := json.Unmarshal(golden, &env); err != nil {
		t.Fatalf("golden is not a valid RawEventEnvelope: %v", err)
	}

	// Top-level fields must be populated (guards json tags against renames).
	if env.Schema != SchemaVersion {
		t.Errorf("schema = %d, want %d", env.Schema, SchemaVersion)
	}
	for name, got := range map[string]string{
		"event_id":    env.EventID,
		"session_id":  env.SessionID,
		"prompt_id":   env.PromptID,
		"tool":        env.Tool,
		"hook_event":  env.HookEvent,
		"captured_at": env.CapturedAt,
		"cli_version": env.CliVersion,
	} {
		if got == "" {
			t.Errorf("top-level field %q did not deserialize (renamed/removed json tag?)", name)
		}
	}
	if len(env.RawEvent) == 0 {
		t.Error("raw_event did not deserialize")
	}
	if len(env.Attachments) != 1 || env.Attachments[0].AttachmentID == "" || env.Attachments[0].Type == "" {
		t.Errorf("attachment did not deserialize: %+v", env.Attachments)
	}

	// Every well-known slug must be present in the golden's enrichments.
	for _, slug := range wellKnownSlugs {
		if _, ok := env.Enrichments[slug]; !ok {
			t.Errorf("golden missing well-known enrichment slug %q", slug)
		}
	}

	// Re-marshal and confirm every top-level key + slug key survives.
	out, err := env.ToJSON()
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "event_id", "session_id", "prompt_id", "tool", "hook_event", "captured_at", "cli_version", "raw_event", "enrichments", "attachments"} {
		if _, ok := got[key]; !ok {
			t.Errorf("top-level key %q missing after round-trip", key)
		}
	}
	var enr map[string]json.RawMessage
	if err := json.Unmarshal(got["enrichments"], &enr); err != nil {
		t.Fatal(err)
	}
	for _, slug := range wellKnownSlugs {
		if _, ok := enr[slug]; !ok {
			t.Errorf("enrichment slug %q missing after round-trip", slug)
		}
	}
}

// TestGoldenEnvelope_SchemaGate mirrors the platform/extension "schema >= 2"
// acceptance gate: the canonical sample must pass it.
func TestGoldenEnvelope_SchemaGate(t *testing.T) {
	var rec struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(loadGolden(t), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Schema < SchemaVersion {
		t.Errorf("golden schema %d < %d — readers would reject it", rec.Schema, SchemaVersion)
	}
}
