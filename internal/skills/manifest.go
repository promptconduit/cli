// Package skills manages locally installed PromptConduit skills.
//
// The manifest at ~/.config/promptconduit/skills-installed.json is the
// source of truth for what we wrote to disk: who put the file there
// (PromptConduit vs the user), when, and whether it's been hand-edited
// since (sha256 comparison against the recorded value).
//
// Uninstall refuses to delete files whose on-disk sha doesn't match the
// manifest entry, unless --force is passed.
package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ManifestVersion is the current on-disk schema version. Bump only on
// backwards-incompatible changes; the array shape is already flexible enough
// to absorb Phase 2 multi-file bundles without a version bump.
const ManifestVersion = 1

// ManifestFileName is the basename of the manifest file inside ConfigDir.
const ManifestFileName = "skills-installed.json"

// Scope identifies where a skill lives on disk. Stored verbatim so future
// formats can extend without ambiguity.
type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
)

// File describes a single file installed as part of a skill.
type File struct {
	// Path is the absolute path to the installed file.
	Path string `json:"path"`
	// SHA256 is the hex-encoded sha256 of the file contents at install time.
	SHA256 string `json:"sha256"`
}

// Entry is one installed skill in the manifest.
type Entry struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Scope          Scope     `json:"scope"`
	InstalledAt    time.Time `json:"installed_at"`
	PlatformSHA256 string    `json:"platform_sha256"`
	Files          []File    `json:"files"`
}

// Manifest is the on-disk record of all PromptConduit-installed skills.
type Manifest struct {
	Version int     `json:"version"`
	Skills  []Entry `json:"skills"`
}

// Path returns the manifest file path inside the given config dir.
func Path(configDir string) string {
	return filepath.Join(configDir, ManifestFileName)
}

// Load reads the manifest from configDir. Returns an empty manifest if the
// file does not exist. A corrupted file returns an error — callers should
// surface that rather than silently dropping recorded skills.
func Load(configDir string) (*Manifest, error) {
	path := Path(configDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Manifest{Version: ManifestVersion}, nil
		}
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	if m.Version == 0 {
		m.Version = ManifestVersion
	}
	return &m, nil
}

// Save writes the manifest atomically. The temp file is created in the same
// directory so os.Rename is a same-filesystem atomic swap.
func Save(configDir string, m *Manifest) error {
	if m == nil {
		return errors.New("nil manifest")
	}
	if m.Version == 0 {
		m.Version = ManifestVersion
	}

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	tmp, err := os.CreateTemp(configDir, ".skills-installed-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails before Rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp manifest: %w", err)
	}

	if err := os.Rename(tmpName, Path(configDir)); err != nil {
		return fmt.Errorf("rename manifest into place: %w", err)
	}
	return nil
}

// Find returns a pointer to the entry with the given name, or nil if not
// tracked. Callers must not mutate the returned pointer's Files slice
// without calling Add to persist; Find is intended for read-only inspection.
func (m *Manifest) Find(name string) *Entry {
	for i := range m.Skills {
		if m.Skills[i].Name == name {
			return &m.Skills[i]
		}
	}
	return nil
}

// Add inserts or replaces the entry keyed by Name. Replacement matches the
// semantics of "I just (re)installed this skill — record the new state."
func (m *Manifest) Add(entry Entry) {
	for i := range m.Skills {
		if m.Skills[i].Name == entry.Name {
			m.Skills[i] = entry
			return
		}
	}
	m.Skills = append(m.Skills, entry)
}

// Remove deletes the entry with the given name. Returns true if an entry
// was removed, false if it wasn't tracked.
func (m *Manifest) Remove(name string) bool {
	for i := range m.Skills {
		if m.Skills[i].Name == name {
			m.Skills = append(m.Skills[:i], m.Skills[i+1:]...)
			return true
		}
	}
	return false
}

// HashContent returns the hex sha256 of data. Used at install time to
// record what we wrote, and at uninstall time to detect hand edits.
func HashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashFile returns the hex sha256 of the file at path.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return HashContent(data), nil
}
