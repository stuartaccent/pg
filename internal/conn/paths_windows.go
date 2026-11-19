//go:build windows

package conn

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// defaultServiceFile is where libpq looks on Windows. pgx also reads
// ~/.pg_service.conf when that's missing, as it once did, so that's used if
// it's the only one there.
func defaultServiceFile() (string, error) {
	path, err := inAppData(".pg_service.conf")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if home, herr := os.UserHomeDir(); herr == nil {
			old := filepath.Join(home, ".pg_service.conf")
			if _, err := os.Stat(old); err == nil {
				return old, nil
			}
		}
	}
	return path, nil
}

func defaultPassFile() (string, error) { return inAppData("pgpass.conf") }

func inAppData(name string) (string, error) {
	dir := os.Getenv("APPDATA")
	if dir == "" {
		return "", errors.New("%APPDATA% isn't set, so there's nowhere to look for Postgres' files: set PGSERVICEFILE and PGPASSFILE")
	}
	return filepath.Join(dir, "postgresql", name), nil
}

// exposed is false: Windows guards files with ACLs, not the mode bits, and
// libpq doesn't check them there. %APPDATA% is the user's own already.
func exposed(fs.FileInfo) bool { return false }
