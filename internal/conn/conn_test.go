package conn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgservicefile"
	"github.com/jackc/pgx/v5/pgconn"
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
		c, err := Open(context.Background(), ServiceDSN(name))
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

func TestExport(t *testing.T) {
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	ctx := context.Background()
	tests := []struct {
		name, query, want string
		wantRows          int64
		wantErr           string
	}{
		{name: "select", query: "select 1 as a, 'x,y' as b", want: "a,b\n1,\"x,y\"\n", wantRows: 1},
		{name: "trailing semicolon", query: "select 1 as a;\n", want: "a\n1\n", wantRows: 1},
		{name: "trailing comment", query: "select 1 as a -- note", want: "a\n1\n", wantRows: 1},
		{name: "null", query: "select null::int as a", want: "a\n\n", wantRows: 1},
		{name: "semicolon then comment", query: "select 1 as a; -- note\n-- another\n", want: "a\n1\n", wantRows: 1},
		{name: "bad sql", query: "select nope", wantErr: "nope"},
		// where false, so this changes nothing even if the guard were to fail.
		{name: "a write is refused", query: "update pg_description set description = description where false returning 1", wantErr: "read-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Open(ctx, ServiceDSN(service))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ctx)
			var out bytes.Buffer
			rows, err := Export(ctx, c, tt.query, &out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want || rows != tt.wantRows {
				t.Errorf("got %q, %d rows", out.String(), rows)
			}
		})
	}
}

func TestEnsureFile(t *testing.T) {
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

	// An existing file keeps its content and its mode.
	if err := os.WriteFile(path, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := EnsureFile(path); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	info, _ = os.Stat(path)
	if string(got) != "keep" || info.Mode().Perm() != 0o640 {
		t.Errorf("file changed: %q, %v", got, info.Mode().Perm())
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

func TestPasswordRejected(t *testing.T) {
	rejected := &pgconn.PgError{Code: "28P01"}
	for err, want := range map[error]bool{
		rejected:                               true,
		fmt.Errorf("connecting: %w", rejected): true,
		&pgconn.PgError{Code: "3D000"}:         false,
		errors.New("connection refused"):       false,
	} {
		if got := PasswordRejected(err); got != want {
			t.Errorf("%v: got %v", err, got)
		}
	}
}

func TestSettingsDatabase(t *testing.T) {
	file := filepath.Join(t.TempDir(), "services")
	t.Setenv("PGSERVICEFILE", file)
	t.Setenv("PGDATABASE", "")
	text := "[named]\nhost=localhost\nuser=me\ndbname=mine\n" +
		"[missing]\nhost=localhost\nuser=me\n" +
		"[blank]\nhost=localhost\nuser=me\ndbname=\n"
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for service, want := range map[string]string{"named": "mine", "missing": "me", "blank": "me"} {
		s, err := SettingsFor(ServiceDSN(service))
		if err != nil {
			t.Fatal(err)
		}
		if s.Database != want {
			t.Errorf("%s: database = %q, want %q", service, s.Database, want)
		}
	}
}
