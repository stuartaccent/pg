package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestFailedConnectResetsTheDropdown(t *testing.T) {
	file := filepath.Join(t.TempDir(), "services")
	t.Setenv("PGSERVICEFILE", file)
	if err := os.WriteFile(file, []byte("[only]\nhost=localhost\nport=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"only", "missing"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/connect", strings.NewReader("service="+service))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		New().Handler().ServeHTTP(w, req)
		body := w.Body.String()
		if !strings.Contains(body, `hx-swap-oob="innerHTML:#service-picker"`) || !strings.Contains(body, `<option value="" disabled selected>`) {
			t.Errorf("%s: dropdown not reset: %q", service, body)
		}
		if w.Header().Get("HX-Trigger") != "" {
			t.Errorf("%s: a failed connect must not reload the tree", service)
		}
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

func TestSandboxOnlyTakesAFileInTheBackupFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, file := range []string{"../../etc/passwd", "/etc/passwd", "missing.backup", "notes.txt", ""} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(url.Values{"file": {file}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s := New()
		s.Handler().ServeHTTP(w, req)
		if s.job != nil {
			t.Errorf("%q started a job", file)
		}
		if !strings.Contains(w.Body.String(), "text-error") {
			t.Errorf("%q: no error shown: %q", file, w.Body.String())
		}
	}
}

func TestRemoveOnlyTakesASandbox(t *testing.T) {
	if got := get(t, New(), http.MethodPost, "/sandboxes/tools-postgres-1/remove").Code; got != http.StatusNotFound {
		t.Errorf("status = %d", got)
	}
}

func TestBackupNeedsAConnection(t *testing.T) {
	s := New()
	if body := get(t, s, http.MethodPost, "/backup").Body.String(); !strings.Contains(body, "connect to a service first") {
		t.Errorf("body = %q", body)
	}
	if s.job != nil {
		t.Error("started a job")
	}
}

func TestOneJobAtATime(t *testing.T) {
	s := New()
	release := make(chan struct{})
	first, err := s.start("first", func(ctx context.Context, progress func(string)) (jobResult, error) {
		<-release
		return jobResult{Text: "done"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.start("second", func(context.Context, func(string)) (jobResult, error) { return jobResult{}, nil }); err == nil {
		t.Error("a second job started")
	}
	close(release)
	for first.view().Running {
		time.Sleep(time.Millisecond)
	}
	if v := first.view(); v.Result.Text != "done" || v.Err != nil {
		t.Errorf("view = %+v", v)
	}

	// The first poll after it finishes tells the page, once.
	if got := get(t, s, http.MethodGet, "/job").Header().Get("HX-Trigger"); got != "services-changed" {
		t.Errorf("first poll trigger = %q", got)
	}
	if got := get(t, s, http.MethodGet, "/job").Header().Get("HX-Trigger"); got != "" {
		t.Errorf("second poll trigger = %q", got)
	}
}

func TestStoppedJobSaysSo(t *testing.T) {
	s := New()
	j, err := s.start("slow", func(ctx context.Context, progress func(string)) (jobResult, error) {
		<-ctx.Done()
		return jobResult{}, errors.New("exit status 143")
	})
	if err != nil {
		t.Fatal(err)
	}
	get(t, s, http.MethodPost, "/job/cancel")
	for j.view().Running {
		time.Sleep(time.Millisecond)
	}
	if v := j.view(); v.Err == nil || v.Err.Error() != "stopped" {
		t.Errorf("err = %v", v.Err)
	}
}

func TestSandboxBadge(t *testing.T) {
	for service, want := range map[string]bool{"pg-sandbox-1a2b": true, "local": false} {
		var b strings.Builder
		if err := ConnectStatus(service, false, nil).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(b.String(), ">sandbox<"); got != want {
			t.Errorf("%s: badge shown = %v", service, got)
		}
	}
}

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{10: "10 B", 2048: "2.0 KB", 33 << 20: "33.0 MB", 5 << 30: "5.0 GB"} {
		if got := humanSize(n); got != want {
			t.Errorf("%d: got %q", n, got)
		}
	}
}

func TestOpenBackupsFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var opened string
	real := openFolder
	openFolder = func(path string) error {
		opened = path
		return nil
	}
	t.Cleanup(func() { openFolder = real })

	if got := get(t, New(), http.MethodPost, "/backups/open").Code; got != http.StatusNoContent {
		t.Errorf("status = %d", got)
	}
	want := filepath.Join(home, "pg-backups")
	if opened != want {
		t.Errorf("opened %q, want %q", opened, want)
	}
	// It's made if need be, as there'd be nothing to open otherwise.
	if info, err := os.Stat(want); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("folder: %v, %v", info, err)
	}

	openFolder = func(string) error { return errors.New("no finder") }
	if body := get(t, New(), http.MethodPost, "/backups/open").Body.String(); !strings.Contains(body, "no finder") {
		t.Errorf("failure not shown: %q", body)
	}
}

func TestVerifyOnlyTakesAFileInTheBackupFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, file := range []string{"../../etc/passwd", "missing.backup", "notes.txt", ""} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/backups/verify", strings.NewReader(url.Values{"file": {file}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s := New()
		s.Handler().ServeHTTP(w, req)
		if s.job != nil {
			t.Errorf("%q started a job", file)
		}
	}
}

func TestDescribeDifferences(t *testing.T) {
	if got := describeDifferences(nil); got != "Matches the live database exactly." {
		t.Errorf("none: %q", got)
	}
	if got := describeDifferences([]string{"a", "b"}); got != "2 differ from the live database: a; b." {
		t.Errorf("few: %q", got)
	}
	many := describeDifferences([]string{"a", "b", "c", "d", "e", "f", "g"})
	if !strings.HasPrefix(many, "7 tables differ") || strings.Contains(many, "f") && strings.Contains(many, "; f") {
		t.Errorf("many: %q", many)
	}
}

func TestDeleteBackup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "pg-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(home, "outside.backup")
	for _, path := range []string{filepath.Join(dir, "a.backup"), filepath.Join(dir, "b.backup"), filepath.Join(dir, "notes.txt"), keep} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	post := func(file string) string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/backups/delete", strings.NewReader(url.Values{"file": {file}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		New().Handler().ServeHTTP(w, req)
		return w.Body.String()
	}

	body := post("a.backup")
	if _, err := os.Stat(filepath.Join(dir, "a.backup")); !os.IsNotExist(err) {
		t.Errorf("a.backup still there: %v", err)
	}
	if strings.Contains(body, "a.backup") || !strings.Contains(body, "b.backup") {
		t.Errorf("list not refreshed: %q", body)
	}

	// Only ever a backup in the folder, whatever the page sends.
	for _, file := range []string{"../outside.backup", keep, "notes.txt", "", ".", "missing.backup"} {
		if body := post(file); !strings.Contains(body, "text-error") {
			t.Errorf("%q: no error shown", file)
		}
	}
	for _, path := range []string{keep, filepath.Join(dir, "notes.txt"), filepath.Join(dir, "b.backup")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}

func TestRestoreRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "pg-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.backup"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	post := func(s *Server, file, database string) string {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/backups/restore", strings.NewReader(url.Values{"file": {file}, "database": {database}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.Handler().ServeHTTP(w, req)
		return w.Body.String()
	}
	connected := func(readOnly bool) *Server {
		s := New()
		s.service, s.readOnly = "local", readOnly
		return s
	}
	for _, tt := range []struct {
		name, file, database, want string
		server                     *Server
	}{
		{"not connected", "a.backup", "ok", "connect to a service first", New()},
		{"read-only", "a.backup", "ok", "local is read-only", connected(true)},
		{"bad name", "a.backup", `x"; drop database y; --`, "not a usable database name", connected(false)},
		{"not in the folder", "../a.backup", "ok", "not a backup", connected(false)},
	} {
		if body := post(tt.server, tt.file, tt.database); !strings.Contains(body, tt.want) && !strings.Contains(body, strings.ReplaceAll(tt.want, `"`, "&#34;")) {
			t.Errorf("%s: %q", tt.name, body)
		}
		if tt.server.job != nil {
			t.Errorf("%s: started a job", tt.name)
		}
	}
}

func TestRestoreButtonNeedsAWritableConnection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := backupDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.backup"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New()
	if body := get(t, s, http.MethodGet, "/backups").Body.String(); strings.Contains(body, ">Restore<") {
		t.Error("not connected: Restore offered")
	}
	s.service = "local"
	if body := get(t, s, http.MethodGet, "/backups").Body.String(); !strings.Contains(body, ">Restore<") {
		t.Error("connected: Restore missing")
	}
	s.readOnly = true
	if body := get(t, s, http.MethodGet, "/backups").Body.String(); strings.Contains(body, ">Restore<") {
		t.Error("read-only: Restore offered")
	}
}

func TestDropdownKeepsARestoredDatabase(t *testing.T) {
	var b strings.Builder
	if err := serviceSelect([]string{"local"}, "local/restored").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `<option value="local/restored" selected>`) {
		t.Errorf("select = %q", b.String())
	}
}
