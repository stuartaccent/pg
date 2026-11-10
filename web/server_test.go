package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/catalog"
	"pg/internal/conn"
	"pg/internal/dockerpg"
	"pg/internal/history"
	"pg/internal/session"
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

func TestActivityNeedsAConnection(t *testing.T) {
	if body := get(t, New(), http.MethodGet, "/activity").Body.String(); !strings.Contains(body, "connect to a service first") {
		t.Errorf("activity = %q", body)
	}
	if body := get(t, New(), http.MethodPost, "/activity/1/terminate").Body.String(); !strings.Contains(body, "connect to a service first") {
		t.Errorf("terminate = %q", body)
	}
}

func TestStopBackendRefuses(t *testing.T) {
	s := New()
	for _, url := range []string{"/activity/x/cancel", "/activity/1/explode"} {
		if got := get(t, s, http.MethodPost, url).Code; got != http.StatusNotFound {
			t.Errorf("%s status = %d", url, got)
		}
	}
	// Past the read-only check, this would report the connect error alone.
	s.readOnly = true
	s.cat = catalog.New(func(context.Context) (*pgconn.PgConn, error) { return nil, errors.New("no server here") })
	if body := get(t, s, http.MethodPost, "/activity/1/terminate").Body.String(); !strings.Contains(body, "read-only") {
		t.Errorf("terminate = %q", body)
	}
}

func TestHealthNeedsAConnection(t *testing.T) {
	if body := get(t, New(), http.MethodGet, "/health").Body.String(); !strings.Contains(body, "connect to a service first") {
		t.Errorf("body = %s", body)
	}
}

func TestHealthBody(t *testing.T) {
	report := catalog.Report{Checks: []catalog.Check{
		{Name: "Found", Reason: "why", SQL: "select 'x'", Columns: []string{"a"}, Rows: [][]string{{"<b>"}}},
		{Name: "Clean", Reason: "why", Usage: true},
		{Name: "Broken", Reason: "why", Err: errors.New("permission denied")},
	}}
	var b bytes.Buffer
	if err := HealthBody(report, nil).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"since the database was made or restored", `>1</span>`, `>none</span>`, `>usage</span>`, "permission denied", "&lt;b&gt;", `data-sql="select &#39;x&#39;"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestActivityButtons(t *testing.T) {
	backends := []catalog.Backend{
		{PID: 1, State: "active", Query: "select 1"},
		{PID: 2, State: "idle"},
	}
	var writable, readOnly strings.Builder
	if err := ActivityBody(backends, true, nil).Render(context.Background(), &writable); err != nil {
		t.Fatal(err)
	}
	if err := ActivityBody(backends, false, nil).Render(context.Background(), &readOnly); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/activity/1/cancel", "/activity/1/terminate", "/activity/2/terminate"} {
		if !strings.Contains(writable.String(), want) {
			t.Errorf("no %s", want)
		}
	}
	if strings.Contains(writable.String(), "/activity/2/cancel") {
		t.Error("an idle connection has nothing to cancel")
	}
	if strings.Contains(readOnly.String(), "/activity/") {
		t.Error("read-only should have no buttons")
	}
}

func postForm(s *Server, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestStartersOnlyArriveOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := New()
	body := get(t, s, http.MethodGet, "/queries").Body.String()
	for _, want := range []string{"Create role", "Server/Overview", "Activity/Long-running queries", "Tables/Cache hit rate", "Tables/Table sizes"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q not listed", want)
		}
	}
	if err := os.Remove(filepath.Join(home, "pg-queries", "Tables", "Table sizes.sql")); err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, http.MethodGet, "/queries").Body.String(); strings.Contains(body, "Table sizes") {
		t.Error("a deleted starter came back")
	}
}

func TestSaveAndReadQuery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := New()
	save := func(name, text, overwrite string) *httptest.ResponseRecorder {
		return postForm(s, "/queries", url.Values{"name": {name}, "text": {text}, "overwrite": {overwrite}})
	}
	read := func() string { return get(t, s, http.MethodGet, "/queries/My%20100%25%20query").Body.String() }

	w := save(" My 100% query.sql ", "select 1", "")
	if w.Code != http.StatusNoContent || w.Header().Get("HX-Trigger") != "query-saved" {
		t.Fatalf("save = %d %q", w.Code, w.Body)
	}
	if got := read(); got != "select 1" {
		t.Errorf("read = %q", got)
	}

	// A taken name asks first, and keeps what's there until told otherwise.
	if w := save("My 100% query", "select 2", ""); !strings.Contains(w.Body.String(), "Overwrite") || w.Header().Get("HX-Trigger") != "" {
		t.Errorf("taken name = %q", w.Body)
	}
	if got := read(); got != "select 1" {
		t.Errorf("after refusal = %q", got)
	}
	save("My 100% query", "select 2", "1")
	if got := read(); got != "select 2" {
		t.Errorf("after overwrite = %q", got)
	}

	if w := save("empty", "  ", ""); !strings.Contains(w.Body.String(), "nothing") {
		t.Errorf("empty tab = %q", w.Body)
	}
	if body := get(t, s, http.MethodGet, "/queries").Body.String(); !strings.Contains(body, `data-url="/queries/My%20100%25%20query"`) {
		t.Errorf("list = %q", body)
	}
}

func TestQueriesStayInTheFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := New()
	for _, name := range []string{"", "../escape", "a/../../escape", "/tmp/escape", "a//b", "a/", ".hidden", "a/.hidden", ".."} {
		w := postForm(s, "/queries", url.Values{"name": {name}, "text": {"select 1"}})
		if !strings.Contains(w.Body.String(), "dot") {
			t.Errorf("save %q = %d %q", name, w.Code, w.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "escape.sql")); err == nil {
		t.Error("wrote outside the folder")
	}
	if err := os.WriteFile(filepath.Join(home, "secret.sql"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"/queries/..%2Fsecret", "/queries/a%2F..%2F..%2Fsecret", "/queries/a/..%2F..%2Fsecret"} {
		if body := get(t, s, http.MethodGet, url).Body.String(); body == "secret" {
			t.Errorf("%s read outside the folder", url)
		}
	}
}

func TestQueriesInSubfolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := New()
	w := postForm(s, "/queries", url.Values{"name": {"stats/deep/Sizes"}, "text": {"select 1"}})
	if w.Code != http.StatusNoContent {
		t.Fatalf("save = %d %q", w.Code, w.Body)
	}
	if body := get(t, s, http.MethodGet, "/queries/stats%2Fdeep%2FSizes").Body.String(); body != "select 1" {
		t.Errorf("read = %q", body)
	}
	git := filepath.Join(home, "pg-queries", ".git")
	if err := os.Mkdir(git, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(git, "inside.sql"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "pg-queries")
	got, err := listQueries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "stats/deep/Sizes") || !slices.Contains(got, "Tables/Table sizes") {
		t.Errorf("list = %q", got)
	}
	if slices.ContainsFunc(got, func(name string) bool { return strings.Contains(name, "inside") }) {
		t.Errorf("listed a hidden folder: %q", got)
	}
}

func TestOpenQueryWithTheDialog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "pg-queries")
	s := New()
	open := func() *httptest.ResponseRecorder { return postForm(s, "/queries/pick", nil) }

	if got := open().Code; got != http.StatusNotImplemented {
		t.Errorf("no dialog = %d", got)
	}

	var started string
	chosen := ""
	s.OpenFile = func(dir string) (string, error) {
		started = dir
		return chosen, nil
	}
	if got := open().Code; got != http.StatusNoContent || started != root {
		t.Errorf("cancelled = %d, started in %q", got, started)
	}

	elsewhere := filepath.Join(t.TempDir(), "migration.sql")
	for path, want := range map[string]string{
		filepath.Join(root, "stats", "Sizes.sql"): "stats/Sizes",
		elsewhere: elsewhere,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("select 'é'"), 0o644); err != nil {
			t.Fatal(err)
		}
		chosen = path
		w := open()
		var got struct{ Name, Text string }
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, w.Code, err)
		}
		if got.Name != want || got.Text != "select 'é'" {
			t.Errorf("%s: got %+v, want name %q", path, got, want)
		}
	}

	chosen = filepath.Join(root, "gone.sql")
	if got := open().Code; got != http.StatusInternalServerError {
		t.Errorf("missing file = %d", got)
	}
}

func TestSaveQueryWithTheDialog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "pg-queries")
	s := New()
	save := func(name, text string) *httptest.ResponseRecorder {
		return postForm(s, "/queries/choose", url.Values{"name": {name}, "text": {text}})
	}

	if got := save("", "select 1").Code; got != http.StatusNotImplemented {
		t.Errorf("no dialog = %d", got)
	}

	var offered [2]string
	chosen := ""
	s.ChooseFile = func(dir, name string) (string, error) {
		offered = [2]string{dir, name}
		return chosen, nil
	}

	// Cancelling writes nothing.
	if w := save("", "select 1"); w.Code != http.StatusNoContent {
		t.Errorf("cancelled = %d", w.Code)
	}
	if filepath.Clean(offered[0]) != root || offered[1] != "Untitled.sql" {
		t.Errorf("offered %q", offered)
	}

	chosen = filepath.Join(root, "stats", "Sizes.sql")
	if w := save("", "select 1"); w.Code != http.StatusOK || w.Body.String() != "stats/Sizes" {
		t.Fatalf("save = %d %q", w.Code, w.Body)
	}

	// Saving again offers the same place, and the dialog has already asked
	// about replacing it.
	if w := save("stats/Sizes", "select 2"); w.Body.String() != "stats/Sizes" {
		t.Errorf("save again = %q", w.Body)
	}
	if filepath.Clean(offered[0]) != filepath.Join(root, "stats") || offered[1] != "Sizes.sql" {
		t.Errorf("offered %q", offered)
	}
	if got, _ := os.ReadFile(chosen); string(got) != "select 2" {
		t.Errorf("file = %q", got)
	}

	// A hint from the page can't move where the dialog starts.
	save("../../etc/passwd", "select 3")
	if filepath.Clean(offered[0]) != root || offered[1] != "Untitled.sql" {
		t.Errorf("offered %q", offered)
	}

	// Without its extension it wasn't what the dialog asked about.
	chosen = filepath.Join(root, "stats", "Sizes")
	if w := save("", "select 4"); w.Code != http.StatusInternalServerError {
		t.Errorf("gained extension = %d %q", w.Code, w.Body)
	}
	if got, _ := os.ReadFile(chosen + ".sql"); string(got) != "select 3" {
		t.Errorf("replaced without asking: %q", got)
	}

	// Elsewhere is allowed, but it's no longer a name in the folder.
	chosen = filepath.Join(home, "elsewhere.sql")
	if w := save("", "select 5"); w.Body.String() != chosen {
		t.Errorf("outside = %q", w.Body)
	}

	if w := save("", "  "); w.Code != http.StatusBadRequest {
		t.Errorf("empty tab = %d", w.Code)
	}
}

func TestOpenQueriesFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var opened string
	real := openFolder
	openFolder = func(path string) error {
		opened = path
		return nil
	}
	t.Cleanup(func() { openFolder = real })
	if got := get(t, New(), http.MethodPost, "/queries/open").Code; got != http.StatusNoContent {
		t.Errorf("status = %d", got)
	}
	if want := filepath.Join(home, "pg-queries"); opened != want {
		t.Errorf("opened %q, want %q", opened, want)
	}
}

// Everything runs in a transaction that's rolled back, so a starter that
// changes something leaves no trace.
func TestStartersRun(t *testing.T) {
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	ctx := context.Background()
	c, err := conn.Open(ctx, conn.ServiceDSN(service))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })
	res, err := c.Exec(ctx, "select current_database()").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	database := string(res[0].Rows[0][0])
	var files []string
	err = fs.WalkDir(starters, "starters", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil || len(files) < 14 {
		t.Fatalf("starters: %v, %v", files, err)
	}
	for _, file := range files {
		text, err := starters.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		sql := strings.ReplaceAll(string(text), "on database mydb", `on database "`+database+`"`)
		_, err = c.Exec(ctx, "begin; "+sql+"; rollback").ReadAll()
		// Slowest queries needs an extension the test server may not have.
		if err != nil && strings.Contains(file, "Slowest") && strings.Contains(err.Error(), "pg_stat_statements") {
			t.Logf("%s: %v", file, err)
			_, _ = c.Exec(ctx, "rollback").ReadAll()
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", file, err)
			_, _ = c.Exec(ctx, "rollback").ReadAll()
		}
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
	connect := func(ctx context.Context) (*pgconn.PgConn, error) { return conn.Open(ctx, conn.ServiceDSN(service)) }

	path, rows, err := saveCSV(context.Background(), connect, "", "select 1 as a", "")
	if err != nil || rows != 1 {
		t.Fatalf("rows = %d, err = %v", rows, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\n1\n" {
		t.Errorf("file = %q", got)
	}

	if _, _, err := saveCSV(context.Background(), connect, "", "select nope", ""); err == nil {
		t.Fatal("bad sql: want an error")
	}
	files, _ := filepath.Glob(filepath.Join(home, "Downloads", "*"))
	if len(files) != 1 {
		t.Errorf("failed export left a file behind: %v", files)
	}

	// A chosen file replaces what's there, but only once the export has worked.
	chosen := filepath.Join(home, "chosen.csv")
	if err := os.WriteFile(chosen, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := saveCSV(context.Background(), connect, "", "select nope", chosen); err == nil {
		t.Fatal("bad sql: want an error")
	}
	if got, _ := os.ReadFile(chosen); string(got) != "old" {
		t.Errorf("failed export changed the file: %q", got)
	}
	if path, _, err := saveCSV(context.Background(), connect, "", "select 2 as b", chosen); err != nil || path != chosen {
		t.Fatalf("path = %q, err = %v", path, err)
	}
	if got, _ := os.ReadFile(chosen); string(got) != "b\n2\n" {
		t.Errorf("file = %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "*.partial")); len(left) != 0 {
		t.Errorf("left a partial file: %v", left)
	}
	if !strings.HasSuffix(path, ".csv") {
		t.Errorf("Downloads file = %s", path)
	}

	// It runs on its own connection, with the tab's search_path.
	if path, _, err := saveCSV(context.Background(), connect, "pg_catalog, information_schema", "select current_schemas(false)::text", chosen); err != nil || path != chosen {
		t.Fatalf("path = %q, err = %v", path, err)
	}
	if got, _ := os.ReadFile(chosen); !strings.Contains(string(got), "{pg_catalog,information_schema}") {
		t.Errorf("search_path not used: %q", got)
	}
}

func TestCancellingTheExportDialog(t *testing.T) {
	s := New()
	s.connect = func(context.Context) (*pgconn.PgConn, error) {
		t.Error("cancelling shouldn't connect")
		return nil, errors.New("no")
	}
	choose := func() *httptest.ResponseRecorder { return postForm(s, "/export/choose", url.Values{"tab": {"1"}}) }
	s.ChooseFile = func(_, _ string) (string, error) { return "", nil }
	if w := choose(); w.Code != http.StatusNoContent || w.Header().Get("HX-Trigger") != "" {
		t.Errorf("cancelled = %d, trigger %q", w.Code, w.Header().Get("HX-Trigger"))
	}
	s.ChooseFile = func(_, _ string) (string, error) { return "", errors.New("no dialog") }
	if body := choose().Body.String(); !strings.Contains(body, "no dialog") {
		t.Errorf("failure not shown: %q", body)
	}
}

func TestExportStartsOnceAPlaceIsChosen(t *testing.T) {
	s := New()
	connected := make(chan struct{}, 1)
	s.connect = func(context.Context) (*pgconn.PgConn, error) {
		connected <- struct{}{}
		return nil, errors.New("no server here")
	}
	dir := t.TempDir()
	s.ChooseFile = func(_, _ string) (string, error) { return filepath.Join(dir, "out.csv"), nil }
	form := url.Values{"tab": {"1"}, "sql": {"select 1"}}

	// The page can't skip the dialog, or name a path of its own.
	if body := postForm(s, "/export", form).Body.String(); !strings.Contains(body, "choose where") {
		t.Errorf("no choice = %q", body)
	}
	w := postForm(s, "/export/choose", form)
	if w.Header().Get("HX-Trigger") != "export-ready" {
		t.Fatalf("trigger = %q", w.Header().Get("HX-Trigger"))
	}
	if strings.Contains(w.Body.String(), dir) {
		t.Error("the path went to the page")
	}
	postForm(s, "/export", form)
	select {
	case <-connected:
	default:
		t.Error("the export didn't start")
	}
	// A choice is good for one export.
	if body := postForm(s, "/export", form).Body.String(); !strings.Contains(body, "choose where") {
		t.Errorf("second export = %q", body)
	}
}

func TestExportWithoutADialogGoesStraightThrough(t *testing.T) {
	s := New()
	s.connect = func(context.Context) (*pgconn.PgConn, error) { return nil, errors.New("no") }
	if got := postForm(s, "/export/choose", url.Values{"tab": {"1"}}).Header().Get("HX-Trigger"); got != "export-ready" {
		t.Errorf("trigger = %q", got)
	}
}

func TestRunningJob(t *testing.T) {
	s := New()
	if got := s.RunningJob(); got != "" {
		t.Errorf("no job = %q", got)
	}
	release := make(chan struct{})
	j, err := s.start("Backup", func(context.Context, func(string)) (jobResult, error) {
		<-release
		return jobResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.RunningJob(); got != "Backup" {
		t.Errorf("running = %q", got)
	}
	close(release)
	<-j.finished
	if got := s.RunningJob(); got != "" {
		t.Errorf("finished = %q", got)
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

func TestOpenPsql(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	home := t.TempDir()
	t.Setenv("HOME", home)
	var scripts []string
	var refuse error
	real, life := openTerminal, envFileLife
	openTerminal = func(script string) error {
		scripts = append(scripts, script)
		return refuse
	}
	envFileLife = 50 * time.Millisecond
	t.Cleanup(func() { openTerminal, envFileLife = real, life })
	passwordFiles := func() int {
		found, _ := filepath.Glob(filepath.Join(tmp, "pg-psql-*.env"))
		return len(found)
	}

	s := New()
	if got := get(t, s, http.MethodPost, "/psql").Body.String(); !strings.Contains(got, "connect to a service first") || len(scripts) != 0 {
		t.Errorf("unconnected: %q, %v", got, scripts)
	}
	s.dsn, s.settings, s.image = "service=x", conn.Settings{Host: "127.0.0.1", Port: 5418, User: "postgres"}, "postgres:18"
	if got := get(t, s, http.MethodPost, "/psql").Code; got != http.StatusNoContent || len(scripts) != 1 {
		t.Fatalf("status = %d, scripts = %v", got, scripts)
	}
	if !strings.Contains(scripts[0], "-v '"+home+"/pg-queries:/queries:ro' -w /queries 'postgres:18' psql; rm -f ") {
		t.Errorf("script = %s", scripts[0])
	}
	// The password file goes once docker has had time to read it, whatever psql does.
	if passwordFiles() != 1 {
		t.Error("password file gone before docker could read it")
	}
	time.Sleep(200 * time.Millisecond)
	if passwordFiles() != 0 {
		t.Error("password file left after docker had time to start")
	}
	// And at once if Terminal never opened.
	refuse = errors.New("not allowed to send Apple events to Terminal")
	if got := get(t, s, http.MethodPost, "/psql").Body.String(); !strings.Contains(got, "not allowed") || passwordFiles() != 0 {
		t.Errorf("refused: %q, %d password files", got, passwordFiles())
	}
}

func TestTerminalScriptEscapes(t *testing.T) {
	want := `do script "echo 'a\\b' \"c\""`
	if got := terminalScript(`echo 'a\b' "c"`); !strings.Contains(got, want) {
		t.Errorf("script = %s\nwant %s", got, want)
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
	// By the row's title, as a running sandbox's source could end in "a.backup".
	if strings.Contains(body, `title="a.backup"`) || !strings.Contains(body, `title="b.backup"`) {
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

func TestReplaceRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "pg-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.backup"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	connected := func(database string, readOnly bool) *Server {
		s := New()
		s.service, s.readOnly, s.settings.Database = "local", readOnly, database
		return s
	}
	for _, tt := range []struct {
		name, file, confirm, want string
		server                    *Server
	}{
		{"not connected", "a.backup", "", "connect to a service first", New()},
		{"read-only", "a.backup", "live", "local is read-only", connected("live", true)},
		{"wrong name typed", "a.backup", "lvie", "doesn&#39;t match", connected("live", false)},
		{"nothing typed", "a.backup", "", "doesn&#39;t match", connected("live", false)},
		{"maintenance database", "a.backup", "postgres", "can&#39;t replace postgres", connected("postgres", false)},
		{"template", "a.backup", "template1", "can&#39;t replace template1", connected("template1", false)},
		{"not in the folder", "../a.backup", "live", "not a backup", connected("live", false)},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/backups/replace", strings.NewReader(url.Values{"file": {tt.file}, "confirm": {tt.confirm}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tt.server.Handler().ServeHTTP(w, req)
		if body := w.Body.String(); !strings.Contains(body, tt.want) {
			t.Errorf("%s: %q", tt.name, body)
		}
		if tt.server.job != nil {
			t.Errorf("%s: started a job", tt.name)
		}
	}
}

func TestReplaceButton(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := backupDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.backup"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New()
	s.service, s.settings.Database = "local", "live"
	if body := get(t, s, http.MethodGet, "/backups").Body.String(); !strings.Contains(body, ">Replace<") {
		t.Error("Replace missing")
	}
	s.settings.Database = "postgres"
	if body := get(t, s, http.MethodGet, "/backups").Body.String(); strings.Contains(body, ">Replace<") {
		t.Error("offered replacing postgres")
	}
	s.settings.Database, s.readOnly = "live", true
	if body := get(t, s, http.MethodGet, "/backups").Body.String(); strings.Contains(body, ">Replace<") {
		t.Error("offered replacing on a read-only service")
	}
	s.readOnly = false
	if err := os.Rename(filepath.Join(dir, "a.backup"), filepath.Join(dir, "live-pg18-20261108-101500"+schemaSuffix)); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, http.MethodGet, "/backups").Body.String()
	if strings.Contains(body, ">Replace<") {
		t.Error("offered replacing with a schema-only backup")
	}
	if !strings.Contains(body, ">Restore<") || !strings.Contains(body, "Schema only") {
		t.Error("schema-only backup missing Restore, or no schema button")
	}
}

func TestStopArrivingTooLateKeepsTheResult(t *testing.T) {
	s := New()
	finishing := make(chan struct{})
	j, err := s.start("swap", func(ctx context.Context, progress func(string)) (jobResult, error) {
		close(finishing)
		<-ctx.Done() // Stop lands here, and the work finishes anyway
		return jobResult{Text: "replaced"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-finishing
	j.cancel()
	<-j.finished
	if v := j.view(); v.Err != nil || v.Result.Text != "replaced" {
		t.Errorf("view = %+v", v)
	}
}

func TestCloseWaitsForAJobToCleanUp(t *testing.T) {
	s := New()
	var cleaned atomic.Bool
	started := make(chan struct{})
	if _, err := s.start("backup", func(ctx context.Context, progress func(string)) (jobResult, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // removing what it made
		cleaned.Store(true)
		return jobResult{}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	s.Close()
	if !cleaned.Load() {
		t.Error("Close returned before the job had cleaned up")
	}
}

func postConnect(s *Server, service string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/connect", strings.NewReader(url.Values{"service": {service}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestConnectToAnotherDatabaseOnAService(t *testing.T) {
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	s := New()
	defer s.Close()
	target := service + "/postgres"
	w := postConnect(s, target)
	if body := w.Body.String(); !strings.Contains(body, "Connected to "+target) {
		t.Fatalf("body = %q", body)
	}
	if got := s.settings.Database; got != "postgres" {
		t.Errorf("settings say %q", got)
	}
	sess := s.session("1")
	if p := sess.Run("select current_database()"); p.Err != nil || p.Rows[0][0].Text != "postgres" {
		t.Fatalf("page = %+v", p)
	}
	// What the page does next: the dropdown has to keep showing where it is.
	if body := get(t, s, http.MethodGet, "/services").Body.String(); !strings.Contains(body, `value="`+target+`" selected`) {
		t.Errorf("services = %q", body)
	}
}

func TestSlashesInConnections(t *testing.T) {
	file := filepath.Join(t.TempDir(), "services")
	t.Setenv("PGSERVICEFILE", file)
	if err := os.WriteFile(file, []byte("[team/app]\nhost=localhost\nport=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The service, and a database on it, reach the connection attempt, which
	// fails on the port, not the name.
	for service, known := range map[string]bool{"team/app": true, "team/app/other": true, "team/other": false} {
		body := postConnect(New(), service).Body.String()
		if got := !strings.Contains(body, "unknown service"); got != known {
			t.Errorf("%s: known = %v: %q", service, got, body)
		}
	}
}

func TestExpiredResultSaysSo(t *testing.T) {
	var b strings.Builder
	p := session.Page{Done: true, NumCols: 3, Total: 500, Err: session.ErrExpired}
	if err := MoreRows(p, "1").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "run it again for the rest") || !strings.Contains(b.String(), `colspan="3"`) {
		t.Errorf("rows = %q", b.String())
	}
}

// app.js finds the query form and Analyze button by these, so renaming one would
// silently stop the warnings before a risky query.
func TestWhatTheWarningsDependOn(t *testing.T) {
	body := get(t, New(), http.MethodGet, "/").Body.String()
	for _, want := range []string{`hx-post="/query"`, `value="analyze"`, `id="confirm"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	js, err := static.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "form[hx-post='/query']") {
		t.Error("app.js no longer looks for the query form")
	}
}

func TestNewBackupFile(t *testing.T) {
	modified := time.Date(2025, 3, 1, 9, 0, 0, 0, time.Local)
	for _, tt := range []struct {
		name, database, version string
		schema                  bool
		taken                   time.Time
	}{
		{"shop-db-next-pg17-20261106-121921.backup", "shop-db-next", "pg17", false, time.Date(2026, 11, 6, 12, 19, 21, 0, time.Local)},
		{"shop_db_next-pg18-20261108-160642-schema.backup", "shop_db_next", "pg18", true, time.Date(2026, 11, 8, 16, 6, 42, 0, time.Local)},
		{"other_app.backup", "other_app", "", false, modified},
		{"x-pg18-2026-schema.backup", "x-pg18-2026-schema", "", false, modified},
	} {
		f := newBackupFile(tt.name, 1536, modified)
		if f.Database != tt.database || f.Version != tt.version || f.Schema != tt.schema || !f.taken.Equal(tt.taken) || f.Size != "1.5 KB" {
			t.Errorf("%s: got %+v", tt.name, f)
		}
	}
	if got := (backupFile{taken: time.Date(2025, 3, 1, 9, 5, 0, 0, time.Local)}).Taken(); got != "1 Mar 2025 09:05" {
		t.Errorf("last year = %q", got)
	}
	if got := (backupFile{taken: time.Date(time.Now().Year(), 3, 1, 9, 5, 0, 0, time.Local)}).Taken(); got != "1 Mar 09:05" {
		t.Errorf("this year = %q", got)
	}
}

func TestSandboxesTable(t *testing.T) {
	var b bytes.Buffer
	sandboxes := []dockerpg.Sandbox{
		{Name: "pg-sandbox-a1", Source: "shop_db-pg17-20261106-074224-schema.backup", Port: 54012, Password: "ab12"},
		{Name: "pg-sandbox-b2", Source: "other_app.backup"},
	}
	if err := BackupsBody("", "", false, "/tmp", nil, sandboxes, nil).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pg-sandbox-a1", ">shop_db", ">pg17<", ">schema<", "6 Nov", "other_app", "/sandboxes/pg-sandbox-b2/remove",
		`popovertarget="connect-pg-sandbox-a1"`, ">54012<", ">ab12<", "postgresql://postgres:ab12@127.0.0.1:54012/sandbox"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestWritesEveryRow(t *testing.T) {
	risky := []string{
		"delete from t",
		"DELETE FROM t;",
		"update t set a = 1",
		"update only t set a = 1;",
		"select 1; delete from t",
		"-- a comment\ndelete from t",
		"/* where */ delete from t",
		"delete from t -- where id = 1",
		"update t set a = 'where'",
		`update t set "where" = 1`,
		"update t set a = $$ where $$",
		"update t set a = $x$ where $x$",
		"update t set a = (select 1 from u where u.id = 1)",
		"with x as (select 1) delete from t",
		"with recursive x as (select 1 where true) update t set a = 1",
		"with x as (delete from t returning *) select * from x",
		"merge into t using s on true when matched then delete",
		"merge into t using s on true when matched then update set a = 1",
	}
	safe := []string{
		"merge into t using s on t.id = s.id when matched then delete",
		"merge into t using s on true when matched and s.gone then delete",
		"merge into t using s on true when not matched then insert values (1)",
		"",
		"select 1",
		"select * from t for update",
		"select * from t for no key update",
		"delete from t where id = 1",
		"update t set a = 1 where id = 1",
		"UPDATE t SET a = 1 WHERE id = 1",
		"insert into t values (1) on conflict (id) do update set a = 1",
		"grant update on t to someone",
		"select 'delete from t'",
		"-- delete from t\nselect 1",
		"create function f() returns void language sql as $$ delete from t $$",
		"delete from t where id = 1; update u set a = 1 where id = 2",
		"delete from t where current of c",
		"delete from", // doesn't parse, so the server says why
	}
	for _, sql := range risky {
		if !writesEveryRow(sql) {
			t.Errorf("%q: want true", sql)
		}
	}
	for _, sql := range safe {
		if writesEveryRow(sql) {
			t.Errorf("%q: want false", sql)
		}
	}
}

func TestCheck(t *testing.T) {
	s := New()
	check := func(sql string) (got struct{ EveryRow bool }) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(url.Values{"sql": {sql}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.Handler().ServeHTTP(w, r)
		if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if check("delete from t where id = 1").EveryRow || !check("delete from t").EveryRow || check("select :id").EveryRow {
		t.Error("everyRow is wrong")
	}
}

func TestHistoryTab(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := New()
	t.Cleanup(s.Close)
	s.service, s.settings.Database = "local", "app"
	for range history.RecentRuns + 2 {
		s.remember(context.Background(), "select * from things where id = :id", "select * from things where id = '1'")
	}

	send := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.Handler().ServeHTTP(w, r)
		return w
	}
	body := send(http.MethodGet, "/history", nil).Body.String()
	for _, want := range []string{"select * from things where id = :id", "<option>local</option>", `hx-get="/history/1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dialog lacks %q", want)
		}
	}
	if body := send(http.MethodGet, "/history/list?q=nothing", nil).Body.String(); !strings.Contains(body, "Nothing found") {
		t.Errorf("search: %s", body)
	}
	if body := send(http.MethodGet, "/history/list?selected=1", nil).Body.String(); !strings.Contains(body, `aria-current="true"`) {
		t.Errorf("the chosen entry isn't marked: %s", body)
	}

	w := send(http.MethodPost, "/history/1/star", url.Values{"starred": {"1"}})
	if w.Code != http.StatusNoContent || w.Header().Get("HX-Trigger") != "history-changed" {
		t.Fatalf("star: %d %q", w.Code, w.Header().Get("HX-Trigger"))
	}
	// The note is already on screen, so History needn't reload to show it.
	if w := send(http.MethodPost, "/history/1/note", url.Values{"note": {"by id"}}); w.Code != http.StatusNoContent || w.Header().Get("HX-Trigger") != "" {
		t.Errorf("note: %d %q", w.Code, w.Header().Get("HX-Trigger"))
	}
	body = send(http.MethodGet, "/history/1", nil).Body.String()
	for _, want := range []string{`aria-label="Unstar"`, "by id", "<td>app</td>", "and 2 earlier runs"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}

	if w := send(http.MethodPost, "/history/clear", url.Values{"days": {"soon"}}); w.Code != http.StatusBadRequest {
		t.Errorf("bad days: %d", w.Code)
	}
	send(http.MethodDelete, "/history/1", nil)
	if body := send(http.MethodGet, "/history/1", nil).Body.String(); !strings.Contains(body, "no longer in history") {
		t.Errorf("after remove: %s", body)
	}
}

func TestHxValsIsJSON(t *testing.T) {
	name := "odd\x01\"name .backup"
	var got map[string]string
	if err := json.Unmarshal([]byte(hxVals("file", name)), &got); err != nil || got["file"] != name {
		t.Errorf("got %v, %v", got, err)
	}
}
