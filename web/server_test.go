package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

func get(t *testing.T, s *Server, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, url, nil))
	return w
}

func TestResponsesAreNotCached(t *testing.T) {
	for _, url := range []string{"/", "/tree?kind=schemas", "/static/app.css"} {
		if got := get(t, New(), http.MethodGet, url).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", url, got)
		}
	}
}

func TestNotConnected(t *testing.T) {
	s := New()
	if got := get(t, s, http.MethodGet, "/tree?kind=schemas").Code; got != http.StatusNoContent {
		t.Errorf("tree status = %d", got)
	}
	if got := get(t, s, http.MethodGet, "/definition?def=view&id=1").Code; got != http.StatusConflict {
		t.Errorf("definition status = %d", got)
	}
	if got := get(t, s, http.MethodGet, "/completions").Body.String(); !strings.Contains(got, `"schemas":null`) {
		t.Errorf("completions = %q", got)
	}
}

func TestNewTabsGetTheirOwnIDs(t *testing.T) {
	s := New()
	get(t, s, http.MethodGet, "/")
	body := get(t, s, http.MethodPost, "/tabs").Body.String()
	for _, want := range []string{`data-tab="2"`, `data-tab-button="2"`, `hx-swap-oob="beforeend:#tabs"`, `#results-2`} {
		if !strings.Contains(body, want) {
			t.Errorf("new tab lacks %q", want)
		}
	}
	if body := get(t, s, http.MethodPost, "/tabs").Body.String(); !strings.Contains(body, `data-tab="3"`) {
		t.Error("second new tab did not get the next id")
	}
}

// htmx appends the input's own id, and the first value wins, so the URL must
// not carry an empty one.
func TestSearchURLLeavesIDToTheInput(t *testing.T) {
	if body := get(t, New(), http.MethodGet, "/").Body.String(); !strings.Contains(body, `hx-get="/tree?kind=search"`) {
		t.Error("search input's URL should have no id")
	}
}

func TestReadOnlyBadge(t *testing.T) {
	for readOnly, want := range map[bool]bool{true: true, false: false} {
		var b strings.Builder
		if err := ConnectStatus("local", readOnly, nil).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(b.String(), "read-only"); got != want {
			t.Errorf("readOnly = %v: badge shown = %v", readOnly, got)
		}
	}
}

func TestSaveCSV(t *testing.T) {
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	// Connecting still needs the real config files, so only Downloads moves.
	services, err := conn.ServiceFilePath()
	if err != nil {
		t.Fatal(err)
	}
	passwords, err := conn.PassFilePath()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGSERVICEFILE", services)
	t.Setenv("PGPASSFILE", passwords)
	home := t.TempDir()
	t.Setenv("HOME", home)
	connect := func(ctx context.Context) (*pgconn.PgConn, error) { return conn.Connect(ctx, service) }

	path, rows, err := saveCSV(context.Background(), connect, "select 1 as a")
	if err != nil || rows != 1 {
		t.Fatalf("rows = %d, err = %v", rows, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\n1\n" {
		t.Errorf("file = %q", got)
	}

	if _, _, err := saveCSV(context.Background(), connect, "select nope"); err == nil {
		t.Fatal("bad sql: want an error")
	}
	files, _ := filepath.Glob(filepath.Join(home, "Downloads", "*"))
	if len(files) != 1 {
		t.Errorf("failed export left a file behind: %v", files)
	}
}

func TestEditConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PGSERVICEFILE", filepath.Join(dir, "services"))
	t.Setenv("PGPASSFILE", filepath.Join(dir, "passwords"))
	var opened []string
	real := openInEditor
	openInEditor = func(path string) error {
		opened = append(opened, path)
		return nil
	}
	t.Cleanup(func() { openInEditor = real })

	s := New()
	for _, file := range []string{"services", "passwords"} {
		if got := get(t, s, http.MethodPost, "/config/"+file).Code; got != http.StatusNoContent {
			t.Errorf("%s status = %d", file, got)
		}
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", file, info, err)
		}
	}
	if len(opened) != 2 {
		t.Errorf("opened = %v", opened)
	}
	if got := get(t, s, http.MethodPost, "/config/.ssh%2Fid_rsa").Code; got != http.StatusNotFound {
		t.Errorf("other file status = %d", got)
	}
	if len(opened) != 2 {
		t.Errorf("opened an unknown file: %v", opened)
	}
}

func TestServicesListsTheFileAgain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "services")
	t.Setenv("PGSERVICEFILE", file)
	if err := os.WriteFile(file, []byte("[added]\nhost=localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if body := get(t, New(), http.MethodGet, "/services").Body.String(); !strings.Contains(body, `value="added"`) {
		t.Errorf("services = %q", body)
	}
}

func TestPasswordHint(t *testing.T) {
	for err, want := range map[error]bool{
		&pgconn.PgError{Code: "28P01"}: true,
		&pgconn.PgError{Code: "3D000"}: false,
	} {
		var b strings.Builder
		if rerr := ConnectStatus("", false, err).Render(context.Background(), &b); rerr != nil {
			t.Fatal(rerr)
		}
		if got := strings.Contains(b.String(), ".pgpass"); got != want {
			t.Errorf("%v: hint shown = %v", err, got)
		}
	}
}

func TestExposedPasswordFileWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgpass")
	t.Setenv("PGPASSFILE", path)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[os.FileMode]bool{0o600: false, 0o644: true} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		if err := ConnectStatus("local", false, nil).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(b.String(), "chmod 600 "+path); got != want {
			t.Errorf("%o: warning shown = %v", mode, got)
		}
	}
}
