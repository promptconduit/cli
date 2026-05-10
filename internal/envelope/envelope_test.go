package envelope

import (
	"encoding/json"
	"testing"
)

func TestNew_MirrorsLegacyFields(t *testing.T) {
	enr := &Enrichment{
		Git: &GitContext{RepoName: "cli", Branch: "main"},
		Correlation: &Correlation{
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
			SpanID:  "00f067aa0ba902b7",
		},
	}
	env := New("dev", "claude-code", "SessionStart", []byte(`{}`), enr)

	if env.Git == nil {
		t.Fatal("expected top-level Git to be mirrored from enrichment")
	}
	if env.Git.RepoName != "cli" {
		t.Errorf("Git.RepoName = %q", env.Git.RepoName)
	}
	if env.Correlation == nil {
		t.Fatal("expected top-level Correlation to be mirrored from enrichment")
	}
	if env.Correlation.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("Correlation.TraceID = %q", env.Correlation.TraceID)
	}
}

func TestToJSON_BothShapesPresent(t *testing.T) {
	enr := &Enrichment{
		Git:         &GitContext{RepoName: "cli", Branch: "main"},
		Source:      "github",
		Correlation: &Correlation{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7"},
		Host:        "host",
		OS:          "linux",
		Arch:        "amd64",
	}
	env := New("dev", "claude-code", "SessionStart", []byte(`{}`), enr)
	out, err := env.ToJSON()
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}

	// Top-level mirrored
	if _, ok := got["git"]; !ok {
		t.Errorf("top-level git missing from wire envelope: %s", out)
	}
	if _, ok := got["correlation"]; !ok {
		t.Errorf("top-level correlation missing from wire envelope: %s", out)
	}

	// Enrichment block present
	gotEnr, ok := got["enrichment"].(map[string]interface{})
	if !ok {
		t.Fatalf("enrichment missing or wrong type: %s", out)
	}
	if _, ok := gotEnr["git"]; !ok {
		t.Errorf("enrichment.git missing")
	}
	if _, ok := gotEnr["correlation"]; !ok {
		t.Errorf("enrichment.correlation missing")
	}
	if gotEnr["source"] != "github" {
		t.Errorf("enrichment.source = %v", gotEnr["source"])
	}
}

func TestNew_NilEnrichment(t *testing.T) {
	env := New("dev", "test", "test", []byte(`{}`), nil)
	if env.Git != nil {
		t.Errorf("expected nil Git when enrichment nil, got %+v", env.Git)
	}
	if env.Correlation != nil {
		t.Errorf("expected nil Correlation when enrichment nil")
	}
}

func TestEnvelopeVersion(t *testing.T) {
	env := New("dev", "test", "test", []byte(`{}`), nil)
	if env.EnvelopeVersion != "1.2" {
		t.Errorf("envelope version = %q, want 1.2", env.EnvelopeVersion)
	}
}
