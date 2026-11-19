package web

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestShortcut(t *testing.T) {
	cases := map[string]string{"⌘O": "Ctrl+O", "⇧⌘F": "Ctrl+Shift+F", "⌘↵": "Ctrl+Enter"}
	for mac, other := range cases {
		want := other
		if runtime.GOOS == "darwin" {
			want = mac
		}
		if got := shortcut(mac); got != want {
			t.Errorf("shortcut(%q) = %q, want %q", mac, got, want)
		}
	}
}

func TestRelativeName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pg-queries")
	cases := map[string]string{
		filepath.Join(root, "Sizes"):          "Sizes",
		filepath.Join(root, "stats", "Sizes"): "stats/Sizes",
		root:                                  "",
		filepath.Join(root, "..", "escape"):   "",
		filepath.Join(t.TempDir(), "other"):   "",
	}
	for path, want := range cases {
		if got := relativeName(root, path); got != want {
			t.Errorf("relativeName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestQueryPathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("\\ and : are ordinary in names elsewhere")
	}
	t.Setenv("USERPROFILE", t.TempDir())
	for _, name := range []string{`a\..\..\escape`, `C:escape`, `stats\Sizes`} {
		if _, err := queryPath(name); err == nil {
			t.Errorf("%q: no error", name)
		}
	}
	if _, err := queryPath("stats/Sizes"); err != nil {
		t.Errorf("stats/Sizes: %v", err)
	}
}
