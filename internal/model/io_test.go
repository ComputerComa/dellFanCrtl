package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveSetsFilePermissionsOnNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := validConfig()
	if err := Save(path, &cfg, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("new config file mode = %o, want 0600", perm)
	}
}

// TestSaveFixesPermissionsOnExistingFile guards against a real bug: a file
// that already exists (e.g. written by an older dellfanctl version before
// mqtt.password made 0600 the right default, or just chmod'd looser by an
// operator) keeps its original mode across os.WriteFile - that call only
// applies the given permission bits when *creating* a file. Save must fix
// the mode explicitly rather than assume WriteFile did.
func TestSaveFixesPermissionsOnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stale: true\n"), 0o644); err != nil {
		t.Fatalf("seeding existing file: %v", err)
	}
	cfg := validConfig()
	if err := Save(path, &cfg, true); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("overwritten config file mode = %o, want 0600", perm)
	}
}

func TestSaveRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := validConfig()
	if err := Save(path, &cfg, false); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := Save(path, &cfg, false); err == nil {
		t.Fatal("expected Save without overwrite to refuse an existing file")
	}
}

func TestLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := validConfig()
	if err := Save(path, &cfg, false); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Sensors) != len(cfg.Sensors) || len(loaded.Groups) != len(cfg.Groups) {
		t.Errorf("round-tripped config mismatch: sensors=%d groups=%d, want sensors=%d groups=%d",
			len(loaded.Sensors), len(loaded.Groups), len(cfg.Sensors), len(cfg.Groups))
	}
}
