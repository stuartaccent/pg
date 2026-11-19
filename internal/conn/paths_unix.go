//go:build !windows

package conn

import (
	"io/fs"
	"os"
	"path/filepath"
)

func defaultServiceFile() (string, error) { return inHome(".pg_service.conf") }

func defaultPassFile() (string, error) { return inHome(".pgpass") }

func inHome(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, name), nil
}

// exposed reports whether anyone but its owner can read the file.
func exposed(info fs.FileInfo) bool { return info.Mode().Perm()&0o077 != 0 }
