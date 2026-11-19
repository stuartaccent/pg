//go:build windows

package conn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPaths(t *testing.T) {
	appData, home := t.TempDir(), t.TempDir()
	t.Setenv("APPDATA", appData)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PGSERVICEFILE", "")
	t.Setenv("PGPASSFILE", "")

	want := filepath.Join(appData, "postgresql", ".pg_service.conf")
	if got, err := ServiceFilePath(); err != nil || got != want {
		t.Errorf("service file = %q, %v; want %q", got, err, want)
	}
	if got, err := PassFilePath(); err != nil || got != filepath.Join(appData, "postgresql", "pgpass.conf") {
		t.Errorf("password file = %q, %v", got, err)
	}

	// ~/.pg_service.conf is used when it's the only one, as pgx does.
	old := filepath.Join(home, ".pg_service.conf")
	if err := os.WriteFile(old, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ServiceFilePath(); got != old {
		t.Errorf("with only ~/.pg_service.conf: %q", got)
	}
	if err := EnsureFile(want); err != nil {
		t.Fatal(err)
	}
	if got, _ := ServiceFilePath(); got != want {
		t.Errorf("with both: %q", got)
	}

	t.Setenv("PGSERVICEFILE", `D:\elsewhere.conf`)
	if got, _ := ServiceFilePath(); got != `D:\elsewhere.conf` {
		t.Errorf("PGSERVICEFILE: %q", got)
	}
}

// Windows has no mode bits for libpq to check, so it's never exposed.
func TestExposedPassFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgpass.conf")
	t.Setenv("PGPASSFILE", path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ExposedPassFile(); got != "" {
		t.Errorf("got %q", got)
	}
}
