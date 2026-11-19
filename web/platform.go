package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"pg/internal/conn"
)

// shortcut is a macOS shortcut, such as ⇧⌘F, as this system writes it: as
// is on a Mac, and Ctrl+Shift+F elsewhere.
func shortcut(mac string) string {
	if runtime.GOOS == "darwin" {
		return mac
	}
	var mods []string
	key := mac
	for _, m := range []struct{ sym, name string }{{"⌘", "Ctrl"}, {"⌥", "Alt"}, {"⇧", "Shift"}} {
		if strings.Contains(key, m.sym) {
			mods = append(mods, m.name)
			key = strings.ReplaceAll(key, m.sym, "")
		}
	}
	if key == "↵" {
		key = "Enter"
	}
	return strings.Join(append(mods, key), "+")
}

// showPath shortens path for showing, writing the home folder as ~, or
// %APPDATA% on Windows, where Postgres' files go.
func showPath(path string) string {
	if dir := os.Getenv("APPDATA"); runtime.GOOS == "windows" && dir != "" {
		if rel, ok := under(path, dir); ok {
			return `%APPDATA%` + rel
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, ok := under(path, home); ok {
			return "~" + rel
		}
	}
	return path
}

// under returns path less the folder dir it's in, starting with a separator.
func under(path, dir string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return string(filepath.Separator) + rel, true
}

// serviceFile is where services go, for showing.
func serviceFile() string {
	path, err := conn.ServiceFilePath()
	if err != nil {
		return "the service file"
	}
	return showPath(path)
}

// passFile is where passwords go, for showing.
func passFile() string {
	path, err := conn.PassFilePath()
	if err != nil {
		return "the password file"
	}
	return showPath(path)
}
