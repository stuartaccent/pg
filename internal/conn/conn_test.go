package conn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgservicefile"
)

// Needs a real server, like the session tests: PGIDE_TEST_SERVICE names a
// service, which is copied into a temp service file with the read-only option.
func TestReadOnly(t *testing.T) {
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	path, err := ServiceFilePath()
	if err != nil {
		t.Fatal(err)
	}
	sf, err := pgservicefile.ReadServicefile(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := sf.GetService(service)
	if err != nil {
		t.Fatal(err)
	}
	text := "[plain]\n"
	for k, v := range svc.Settings {
		text += fmt.Sprintf("%s=%s\n", k, v)
	}
	text += "[locked]\noptions=-c default_transaction_read_only=on\n"
	for k, v := range svc.Settings {
		text += fmt.Sprintf("%s=%s\n", k, v)
	}
	file := filepath.Join(t.TempDir(), "services.conf")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", file)

	for name, want := range map[string]bool{"plain": false, "locked": true} {
		c, err := Connect(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if got := ReadOnly(c); got != want {
			t.Errorf("%s: ReadOnly = %v, want %v", name, got, want)
		}
		_, err = c.Exec(context.Background(), "create temp table should_fail_if_locked (id int)").ReadAll()
		if (err != nil) != want {
			t.Errorf("%s: writing gave err = %v", name, err)
		}
		_ = c.Close(context.Background())
	}
}
