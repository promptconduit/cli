package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/promptconduit/cli/internal/eventlog"
)

// markerFileName is the drop file the CLI writes after it updates the editor's
// installed cost extension on disk. The running extension watches it and, when
// the recorded version is newer than the version it's currently running, offers
// a one-click **Reload Window** (not a restart) to apply the update. It lives in
// ~/.promptconduit/ next to events.jsonl — the folder the extension already
// watches — so detection is nearly free.
const markerFileName = "extension-update.json"

// UpdateMarker is the on-disk contract shared with the editor extension. Keep
// the JSON field names in sync with editor-extension/src/updatePrompt.ts.
type UpdateMarker struct {
	Version   string `json:"version"`    // extension version now installed on disk
	Editor    string `json:"editor"`     // human label, e.g. "Cursor"
	UpdatedAt string `json:"updated_at"` // RFC3339 UTC timestamp of the update
}

// MarkerPath is the absolute path of the update marker file.
func MarkerPath() string { return filepath.Join(eventlog.Dir(), markerFileName) }

// WriteUpdateMarker records that the on-disk cost extension was updated to
// version for editor, so a running extension can detect the drift and prompt a
// reload. Written atomically (temp file + rename) so the extension never reads a
// half-written file. Best-effort: the returned error is for the caller to log or
// ignore — a failed marker only means the user misses the in-editor toast, not
// that the update itself failed.
func WriteUpdateMarker(version, editor string) error {
	dir := eventlog.Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(UpdateMarker{
		Version:   version,
		Editor:    editor,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, markerFileName+"-*.tmp")
	if err != nil {
		// Fall back to a direct write rather than dropping the marker.
		return os.WriteFile(MarkerPath(), data, 0o644)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, MarkerPath()); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
