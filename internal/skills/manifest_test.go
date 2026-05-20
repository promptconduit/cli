package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_MissingFile_ReturnsEmptyManifest(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load on empty dir: %v", err)
	}
	if m.Version != ManifestVersion {
		t.Errorf("Version = %d, want %d", m.Version, ManifestVersion)
	}
	if len(m.Skills) != 0 {
		t.Errorf("Skills = %v, want empty", m.Skills)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	original := &Manifest{
		Version: ManifestVersion,
		Skills: []Entry{
			{
				ID:             "uuid-1",
				Name:           "shipping-features",
				Scope:          ScopeGlobal,
				InstalledAt:    now,
				PlatformSHA256: "abc",
				Files: []File{
					{Path: "/home/me/.claude/skills/shipping-features/SKILL.md", SHA256: "abc"},
				},
			},
		},
	}

	if err := Save(dir, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Skills) != 1 {
		t.Fatalf("len(Skills) = %d, want 1", len(loaded.Skills))
	}
	got := loaded.Skills[0]
	if got.ID != "uuid-1" || got.Name != "shipping-features" || got.Scope != ScopeGlobal {
		t.Errorf("entry mismatch: %+v", got)
	}
	if !got.InstalledAt.Equal(now) {
		t.Errorf("InstalledAt = %v, want %v", got.InstalledAt, now)
	}
	if got.PlatformSHA256 != "abc" || len(got.Files) != 1 || got.Files[0].SHA256 != "abc" {
		t.Errorf("hashes/files mismatch: %+v", got)
	}
}

func TestSave_IsAtomic_NoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{Skills: []Entry{{Name: "x"}}}
	if err := Save(dir, m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != ManifestFileName {
			t.Errorf("leftover file in config dir: %s", e.Name())
		}
	}
}

func TestSave_OverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{Skills: []Entry{{Name: "first"}}}
	if err := Save(dir, m); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	m.Skills = []Entry{{Name: "second"}}
	if err := Save(dir, m); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0].Name != "second" {
		t.Errorf("Skills = %+v, want [{second}]", loaded.Skills)
	}
}

func TestFind_HitAndMiss(t *testing.T) {
	m := &Manifest{
		Skills: []Entry{
			{Name: "alpha"}, {Name: "beta"},
		},
	}
	if got := m.Find("alpha"); got == nil || got.Name != "alpha" {
		t.Errorf("Find(alpha) = %+v, want entry", got)
	}
	if got := m.Find("missing"); got != nil {
		t.Errorf("Find(missing) = %+v, want nil", got)
	}
}

func TestAdd_InsertsThenReplaces(t *testing.T) {
	m := &Manifest{}
	m.Add(Entry{Name: "x", PlatformSHA256: "v1"})
	m.Add(Entry{Name: "y", PlatformSHA256: "v1"})
	m.Add(Entry{Name: "x", PlatformSHA256: "v2"})
	if len(m.Skills) != 2 {
		t.Fatalf("len = %d, want 2 (replace, not duplicate)", len(m.Skills))
	}
	got := m.Find("x")
	if got == nil || got.PlatformSHA256 != "v2" {
		t.Errorf("expected x at v2, got %+v", got)
	}
}

func TestRemove_PresentAndAbsent(t *testing.T) {
	m := &Manifest{
		Skills: []Entry{{Name: "a"}, {Name: "b"}, {Name: "c"}},
	}
	if !m.Remove("b") {
		t.Error("Remove(b) = false, want true")
	}
	if len(m.Skills) != 2 || m.Find("b") != nil {
		t.Errorf("after Remove(b): %+v", m.Skills)
	}
	if m.Remove("missing") {
		t.Error("Remove(missing) = true, want false")
	}
}

func TestLoad_CorruptFile_Errors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("Load on corrupt file: want error, got nil")
	}
}

func TestLoad_VersionZero_DefaultsToCurrent(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"skills": []any{}})
	if err := os.WriteFile(Path(dir), raw, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Version != ManifestVersion {
		t.Errorf("Version = %d, want %d", m.Version, ManifestVersion)
	}
}

func TestHashContent_Stable(t *testing.T) {
	want := "c3ab8ff13720e8ad9047dd39466b3c8974e592c2fa383d4a3960714caef0c4f2"
	if got := HashContent([]byte("foobar")); got != want {
		t.Errorf("HashContent(foobar) = %q, want %q", got, want)
	}
}

func TestHashFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	body := []byte("hello world")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if got != HashContent(body) {
		t.Errorf("HashFile != HashContent for same bytes")
	}
}
