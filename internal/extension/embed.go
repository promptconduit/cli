// Package extension bundles the PromptConduit cost editor extension (a VS Code /
// Cursor .vsix) directly into the CLI binary, so `promptconduit install cursor`
// can sideload it with no marketplace, no tokens, and a version locked to the
// CLI's cost-feed schema.
//
// embedded/promptconduit-cost.vsix is a build artifact from the
// promptconduit/editor-extension repo. Regenerate it with `make refresh-extension`
// after the extension changes — see that target.
package extension

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed embedded/promptconduit-cost.vsix
var vsix []byte

// VSIXFileName is the filename used when the embedded extension is written to
// disk for an editor's `--install-extension` CLI to read.
const VSIXFileName = "promptconduit-cost.vsix"

// Bytes returns the embedded .vsix contents (the bytes we write to a temp file
// and hand to the editor CLI).
func Bytes() []byte { return vsix }

var (
	versionOnce sync.Once
	versionVal  string
	versionErr  error
)

// Version returns the extension version baked into the embedded .vsix, read from
// its extension/package.json so it can never drift from the artifact we ship.
// Result is cached.
func Version() (string, error) {
	versionOnce.Do(func() {
		versionVal, versionErr = readVSIXVersion(vsix)
	})
	return versionVal, versionErr
}

// readVSIXVersion reads `version` from the extension/package.json inside the
// .vsix (a zip archive).
func readVSIXVersion(b []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", fmt.Errorf("open embedded vsix: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "extension/package.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		var pkg struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(rc).Decode(&pkg); err != nil {
			return "", fmt.Errorf("decode extension package.json: %w", err)
		}
		if pkg.Version == "" {
			return "", fmt.Errorf("extension package.json has no version")
		}
		return pkg.Version, nil
	}
	return "", fmt.Errorf("extension/package.json not found in embedded vsix")
}
