package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"

	"github.com/promptconduit/cli/internal/client"
	"github.com/promptconduit/cli/internal/sync"
)

// TestSingleFileSyncCarriesSubagents: subagents and workflow agents never fire
// a Stop hook of their own, so the parent session's auto-sync must upload
// their transcripts too: only the ones that changed, and never recursing from
// a subagent file.
func TestSingleFileSyncCarriesSubagents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	var mu gosync.Mutex
	var uploads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uploads = append(uploads, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.TranscriptSyncResponse{ConversationID: "c", MessageCount: 1, Status: "created"})
	}))
	defer srv.Close()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(uploads)
	}

	dir := t.TempDir()
	line := func(text string) []byte {
		return []byte(`{"type":"user","sessionId":"sess","uuid":"u-` + text + `","timestamp":"2026-11-14T10:00:00Z","message":{"role":"user","content":"` + text + `"}}` + "\n")
	}
	main := filepath.Join(dir, "sess.jsonl")
	sub := filepath.Join(dir, "sess", "subagents", "agent-a.jsonl")
	wf := filepath.Join(dir, "sess", "subagents", "workflows", "wf_1", "agent-b.jsonl")
	for _, p := range []string{main, sub, wf} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, line(filepath.Base(p)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &client.Config{APIKey: "k", APIURL: srv.URL, TimeoutSeconds: 5}
	sm, _ := sync.NewStateManager()

	if err := runSingleFileSync(cfg, sm, main); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if got := count(); got != 3 {
		t.Fatalf("first sync uploaded %d transcripts, want 3 (main, subagent, workflow agent): %v", got, uploads)
	}
	for _, p := range []string{main, sub, wf} {
		if _, ok := sm.GetSyncedInfo(p); !ok {
			t.Errorf("%s not marked synced", p)
		}
	}

	// Nothing changed: nothing is uploaded again.
	if err := runSingleFileSync(cfg, sm, main); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := count(); got != 3 {
		t.Fatalf("unchanged files re-uploaded: %d uploads, want 3", got)
	}

	// Only the subagent changed: only it is uploaded.
	if err := os.WriteFile(sub, append(line("a"), line("b")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runSingleFileSync(cfg, sm, main); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if got := count(); got != 4 {
		t.Fatalf("after a subagent change: %d uploads, want 4", got)
	}

	// Syncing a subagent file directly never walks for nested subagents.
	if !isSubagentTranscript(sub) || isSubagentTranscript(main) {
		t.Fatal("isSubagentTranscript misclassified a path")
	}
}

// TestUnchangedSinceSyncFingerprint: the skip needs an exact mtime+size match
// with the fingerprint taken before hashing. An append (even one landing in
// the same coarse mtime tick) changes the size, so it is never skipped.
func TestUnchangedSinceSyncFingerprint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	path := filepath.Join(t.TempDir(), "agent-a.jsonl")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	sm, _ := sync.NewStateManager()

	sm.MarkSynced(path, sync.SyncedFileInfo{Hash: "h"})
	if unchangedSinceSync(sm, path) {
		t.Fatal("a record without a fingerprint must fall through to the hash check")
	}

	sm.MarkSynced(path, sync.SyncedFileInfo{Hash: "h", SourceModTime: st.ModTime().UnixNano(), SourceSize: st.Size()})
	if !unchangedSinceSync(sm, path) {
		t.Fatal("matching mtime+size should skip")
	}

	// Append, then force the old mtime back: same tick, bigger file.
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if unchangedSinceSync(sm, path) {
		t.Fatal("a size change with the same mtime must not be skipped")
	}
}
