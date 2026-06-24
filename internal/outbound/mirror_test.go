package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMirror_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)
	m := New(path, http.DefaultTransport)

	client := &http.Client{Transport: m}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/events/raw", bytes.NewReader([]byte(`{"hook_event_name":"UserPromptSubmit"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk_live_supersecret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != `{"ok":true}` {
		t.Errorf("downstream caller saw wrong body: %q", body)
	}

	// File should exist with one ndjson line.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("mirror line should end with newline")
	}
	var entry Entry
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatalf("ndjson parse: %v", err)
	}
	if entry.Method != "POST" {
		t.Errorf("method = %q, want POST", entry.Method)
	}
	if !strings.HasSuffix(entry.URL, "/v1/events/raw") {
		t.Errorf("url = %q, want suffix /v1/events/raw", entry.URL)
	}
	if entry.Status != 200 {
		t.Errorf("status = %d, want 200", entry.Status)
	}
	if entry.ReqBody == "" {
		t.Error("expected request body to be recorded")
	}
	if entry.ReqHeaders["Authorization"] != redactedValue {
		t.Errorf("authorization not redacted: %q", entry.ReqHeaders["Authorization"])
	}
	if strings.Contains(string(data), "sk_live_supersecret") {
		t.Error("raw bearer token leaked to mirror file")
	}
}

func TestMirror_FilePermissions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)
	m := New(path, http.DefaultTransport)
	client := &http.Client{Transport: m}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// On Unix the file should be created with mode 0600. On Windows the
	// concept doesn't apply the same way, so just assert "not world-rw".
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		t.Errorf("file mode %o is world-readable; expected owner-only", mode)
	}
}

func TestMirror_BodyTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)
	m := New(path, http.DefaultTransport)
	m.MaxBodyBytes = 16 // tiny cap so we can hit truncation easily

	body := strings.Repeat("x", 100)
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
	if _, err := (&http.Client{Transport: m}).Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var entry Entry
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !entry.ReqTruncated {
		t.Error("expected ReqTruncated=true")
	}
	if entry.ReqOriginalSize != 100 {
		t.Errorf("ReqOriginalSize = %d, want 100", entry.ReqOriginalSize)
	}
	if len(entry.ReqBody) != 16 {
		t.Errorf("ReqBody = %d bytes, want 16", len(entry.ReqBody))
	}
}

func TestMirror_Rotates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)
	m := New(path, http.DefaultTransport)
	m.RotateAt = 512 // tiny so we rotate quickly

	client := &http.Client{Transport: m}
	for i := 0; i < 20; i++ {
		body := fmt.Sprintf(`{"i":%d,"pad":"%s"}`, i, strings.Repeat("p", 100))
		req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
		if _, err := client.Do(req); err != nil {
			t.Fatalf("Do: %v", err)
		}
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected backup file at %s: %v", path+".1", err)
	}
}

func TestMirror_RequestBodyStillReadableDownstream(t *testing.T) {
	// The mirror reads the request body to record it; we must replace
	// it so the inner transport still sees the bytes. Tighten that
	// invariant with an explicit test against an httptest.Server that
	// echoes what it received.
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	m := New(filepath.Join(dir, MirrorFileName), http.DefaultTransport)
	client := &http.Client{Transport: m}

	want := `{"hook_event_name":"Stop","session_id":"abc"}`
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(want))
	if _, err := client.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(seen) != want {
		t.Errorf("server received %q, want %q", seen, want)
	}
}

func TestMirror_NetworkErrorRecorded(t *testing.T) {
	// Point at a closed server so RoundTrip fails. Mirror should still
	// record an entry with the error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately

	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)
	m := New(path, http.DefaultTransport)
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(`{}`))
	if _, err := (&http.Client{Transport: m}).Do(req); err == nil {
		t.Fatal("expected request to a closed server to fail")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if !bytes.Contains(data, []byte(`"error":`)) {
		t.Errorf("expected error field in entry; got %s", data)
	}
}

func TestTail_BackfillAndLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)

	// Seed with three lines.
	mustAppend := func(line string) {
		t.Helper()
		if err := appendLine(path, []byte(line)); err != nil {
			t.Fatalf("appendLine: %v", err)
		}
	}
	mustAppend(`{"i":1}`)
	mustAppend(`{"i":2}`)
	mustAppend(`{"i":3}`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch := Tail(ctx, path, 2)

	// Expect the last two backfilled lines.
	got1 := waitLine(t, ch)
	got2 := waitLine(t, ch)
	if string(got1) != `{"i":2}` || string(got2) != `{"i":3}` {
		t.Errorf("backfill = %q, %q; want {i:2}, {i:3}", got1, got2)
	}

	// Append a new line; it should arrive.
	mustAppend(`{"i":4}`)
	got3 := waitLine(t, ch)
	if string(got3) != `{"i":4}` {
		t.Errorf("live = %q; want {i:4}", got3)
	}
}

func TestTail_HandlesRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, MirrorFileName)
	mustAppend := func(line string) {
		t.Helper()
		if err := appendLine(path, []byte(line)); err != nil {
			t.Fatalf("appendLine: %v", err)
		}
	}

	mustAppend(`{"pre":1}`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := Tail(ctx, path, 1)

	// Backfill: {"pre":1}
	if got := waitLine(t, ch); string(got) != `{"pre":1}` {
		t.Fatalf("backfill = %q", got)
	}

	// Simulate rotation: rename to .1, then create a fresh file with
	// new content.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatalf("rotate rename: %v", err)
	}
	mustAppend(`{"post":1}`)

	// Tail should detect the rotation and yield the new line.
	got := waitLine(t, ch)
	if string(got) != `{"post":1}` {
		t.Errorf("after rotate = %q; want {post:1}", got)
	}
}

func waitLine(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case l, ok := <-ch:
		if !ok {
			t.Fatal("tail channel closed unexpectedly")
		}
		return l
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tail line")
	}
	return nil
}
