//go:build !windows

package conn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".pgpass")
	if err := EnsureFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}

	// An existing file keeps its mode.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFile(path); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode changed: %v", info.Mode().Perm())
	}
}

func TestExposedPassFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgpass")
	t.Setenv("PGPASSFILE", path)
	if got := ExposedPassFile(); got != "" {
		t.Errorf("missing file: %q", got)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]string{0o600: "", 0o400: "", 0o640: path, 0o644: path, 0o604: path} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if got := ExposedPassFile(); got != want {
			t.Errorf("%o: got %q, want %q", mode, got, want)
		}
	}
}
