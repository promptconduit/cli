package correlation

import (
	"testing"
)

func TestNewTraceID_Format(t *testing.T) {
	id := NewTraceID()
	if len(id) != 32 {
		t.Fatalf("trace id length = %d, want 32", len(id))
	}
	if !IsValidTraceID(id) {
		t.Fatalf("trace id %q failed validation", id)
	}
}

func TestNewSpanID_Format(t *testing.T) {
	id := NewSpanID()
	if len(id) != 16 {
		t.Fatalf("span id length = %d, want 16", len(id))
	}
	if !IsValidSpanID(id) {
		t.Fatalf("span id %q failed validation", id)
	}
}

func TestNewTraceID_Uniqueness(t *testing.T) {
	const n = 100000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewTraceID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate trace id at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewSpanID_Uniqueness(t *testing.T) {
	const n = 100000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewSpanID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate span id at iteration %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestIsValidTraceID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"4bf92f3577b34da6a3ce929d0e0e4736", true},
		{"00000000000000000000000000000000", false}, // all-zero
		{"4BF92F3577B34DA6A3CE929D0E0E4736", false}, // uppercase
		{"4bf92f3577b34da6a3ce929d0e0e473", false},  // 31 chars
		{"4bf92f3577b34da6a3ce929d0e0e47366", false}, // 33 chars
		{"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", false}, // non-hex
		{"", false},
	}
	for _, tc := range cases {
		got := IsValidTraceID(tc.in)
		if got != tc.want {
			t.Errorf("IsValidTraceID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsValidSpanID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"00f067aa0ba902b7", true},
		{"0000000000000000", false},
		{"00F067AA0BA902B7", false},
		{"00f067aa0ba902b", false},
		{"00f067aa0ba902b77", false},
		{"", false},
	}
	for _, tc := range cases {
		got := IsValidSpanID(tc.in)
		if got != tc.want {
			t.Errorf("IsValidSpanID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
