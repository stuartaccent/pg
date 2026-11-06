package dockerpg

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pg/internal/conn"
)

func TestImage(t *testing.T) {
	for version, want := range map[string]string{
		"17.2 (Debian 17.2-1.pgdg120+1)": "postgres:17",
		"16.4":                           "postgres:16",
		"":                               "postgres:latest",
		"beta":                           "postgres:latest",
	} {
		if got := Image(version); got != want {
			t.Errorf("Image(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestBackupNamesRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 5, 21, 30, 0, 0, time.UTC)
	name := BackupName("crm/db production", "postgres:17", at)
	if want := "crm_db_production-pg17-20261105-213000.backup"; name != want {
		t.Errorf("name = %q, want %q", name, want)
	}
	if got := ImageForBackup(name); got != "postgres:17" {
		t.Errorf("image = %q", got)
	}
	if got := ImageForBackup("other_app.backup"); got != "postgres:latest" {
		t.Errorf("image for someone else's file = %q", got)
	}
}

func TestContainerHost(t *testing.T) {
	for host, want := range map[string]string{
		"localhost":      "host.docker.internal",
		"127.0.0.1":      "host.docker.internal",
		"::1":            "host.docker.internal",
		"db.example.com": "db.example.com",
	} {
		if got := containerHost(host); got != want {
			t.Errorf("%s: got %q", host, got)
		}
	}
}

func TestParseSandboxes(t *testing.T) {
	out := "pg-sandbox-1\tsecret\ta.backup\t127.0.0.1:55012->5432/tcp\n" +
		"pg-sandbox-2\tother\tb.backup\t5432/tcp\n" +
		"not a sandbox line\n"
	got := parseSandboxes(out)
	if len(got) != 1 || got[0] != (Sandbox{Name: "pg-sandbox-1", Password: "secret", Source: "a.backup", Port: 55012}) {
		t.Errorf("got %+v", got)
	}
}

func TestPortOf(t *testing.T) {
	if got, err := portOf("127.0.0.1:55012\n"); err != nil || got != 55012 {
		t.Errorf("got %d, %v", got, err)
	}
	if _, err := portOf("nonsense"); err == nil {
		t.Error("want an error")
	}
}

// settings returns a test service's details, or skips. Needs Docker as well as
// the server the other tests need.
func settings(t *testing.T) (conn.Settings, string) {
	t.Helper()
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" || testing.Short() {
		t.Skip("PGIDE_TEST_SERVICE not set, or -short")
	}
	ctx := context.Background()
	if _, err := Sandboxes(ctx); err != nil {
		t.Skipf("no Docker: %v", err)
	}
	dsn := conn.ServiceDSN(service)
	s, err := conn.SettingsFor(dsn)
	if err != nil {
		t.Fatal(err)
	}
	c, err := conn.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	return s, Image(c.ParameterStatus("server_version"))
}

// TestBackupThenSandbox is the whole journey, on a small sandbox of its own:
// back one up, then restore that into another and read the data back.
func TestBackupThenSandbox(t *testing.T) {
	_, image := settings(t)
	ctx := context.Background()
	quiet := func(string) {}

	// A source with something in it, made from an empty dump of nothing.
	dir := t.TempDir()
	source, cleanup := emptySandbox(t, image)
	defer cleanup()
	seed, err := conn.Open(ctx, source.DSN())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, "create table kept (id int primary key, label text); insert into kept values (1, 'it worked')").ReadAll(); err != nil {
		t.Fatal(err)
	}
	seed.Close(ctx)

	s, err := conn.SettingsFor(source.DSN())
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, BackupName("sandbox", image, time.Now()))
	var lines int
	if err := Backup(ctx, s, image, dest, func(string) { lines++ }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() == 0 || lines == 0 {
		t.Errorf("mode = %v, size = %d, progress lines = %d", info.Mode().Perm(), info.Size(), lines)
	}

	sb, ignored, err := NewSandbox(ctx, ImageForBackup(filepath.Base(dest)), dest, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer Remove(ctx, sb.Name)
	if ignored != 0 {
		t.Errorf("ignored = %d", ignored)
	}

	found, err := Sandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, f := range found {
		if f == sb {
			listed = true
		}
	}
	if !listed {
		t.Errorf("%+v not in %+v", sb, found)
	}

	c, err := conn.Open(ctx, sb.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	res, err := c.Exec(ctx, "select label from kept").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res[0].Rows[0][0]); got != "it worked" {
		t.Errorf("label = %q", got)
	}

	inspect, err := output(ctx, nil, "port", sb.Name, "5432/tcp")
	if err != nil || inspect[:10] != "127.0.0.1:" {
		t.Errorf("port = %q, %v", inspect, err)
	}
}

func TestFailedBackupLeavesNothing(t *testing.T) {
	_, image := settings(t)
	dest := filepath.Join(t.TempDir(), "x.backup")
	bad := conn.Settings{Host: "127.0.0.1", Port: 1, User: "nobody", Database: "nothing"}
	if err := Backup(context.Background(), bad, image, dest, func(string) {}); err == nil {
		t.Fatal("want an error")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), "*"))
	if len(files) != 0 {
		t.Errorf("left behind: %v", files)
	}
}

func TestFailedRestoreIsRemoved(t *testing.T) {
	_, image := settings(t)
	backup := filepath.Join(t.TempDir(), "x.backup")
	if err := os.WriteFile(backup, []byte("not a real dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := Sandboxes(context.Background())
	if _, _, err := NewSandbox(context.Background(), image, backup, func(string) {}); err == nil {
		t.Fatal("a bad file: want an error")
	}
	after, _ := Sandboxes(context.Background())
	if len(after) != len(before) {
		t.Errorf("sandbox left running: %+v", after)
	}
}

func TestStoppingASandboxRemovesIt(t *testing.T) {
	_, image := settings(t)
	backup := filepath.Join(t.TempDir(), "x.backup")
	if err := os.WriteFile(backup, []byte("not a real dump"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := Sandboxes(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := NewSandbox(ctx, image, backup, func(line string) {
		if line == "Waiting for Postgres to start" {
			cancel()
		}
	})
	if err == nil {
		t.Fatal("want an error")
	}
	after, _ := Sandboxes(context.Background())
	if len(after) != len(before) {
		t.Errorf("sandbox left running: %+v", after)
	}
}

func TestCountListedTables(t *testing.T) {
	list := `;
; Archive created at 2026-11-05
;
215; 1259 16385 TABLE public kept postgres
216; 1259 16390 TABLE public other postgres
217; 1259 16395 SEQUENCE public other_id_seq postgres
3341; 0 16385 TABLE DATA public kept postgres
3342; 0 16390 TABLE DATA public other postgres
218; 1259 16399 TABLE public DATA postgres
3100; 0 0 TABLE ATTACH public parted_low postgres
`
	if got := countListedTables(list); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestReportProblem(t *testing.T) {
	counts := map[string]int64{"public.a": 2, "public.b": 3}
	for _, tt := range []struct {
		report Report
		want   string
	}{
		{Report{ListedTables: 2, Counts: counts}, ""},
		{Report{ListedTables: 3, Counts: counts}, "the backup lists 3 tables, but 2 were restored"},
		{Report{ListedTables: 2, Counts: counts, Ignored: 4}, "the restore skipped 4 errors"},
	} {
		if got := tt.report.Problem(); got != tt.want {
			t.Errorf("%+v: got %q", tt.report, got)
		}
	}
	if got := (Report{Counts: counts}).Rows(); got != 5 {
		t.Errorf("rows = %d", got)
	}
}

func TestDifferences(t *testing.T) {
	backup := map[string]int64{"public.same": 1, "public.grew": 2, "public.dropped": 3}
	live := map[string]int64{"public.same": 1, "public.grew": 5, "public.added": 4}
	want := []string{
		"public.added: not in the backup",
		"public.dropped: no longer in the live database",
		"public.grew: 2 rows in the backup, 5 live",
	}
	if got := Differences(backup, live); !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	if got := Differences(live, live); len(got) != 0 {
		t.Errorf("identical: %q", got)
	}
}

func TestSameDatabase(t *testing.T) {
	name := BackupName("crm/db", "postgres:17", time.Now())
	if !SameDatabase(name, "crm/db") {
		t.Error("same database not matched")
	}
	for _, other := range []string{"crm", "crm_db2", ""} {
		if SameDatabase(name, other) {
			t.Errorf("matched %q", other)
		}
	}
}

func TestVerify(t *testing.T) {
	_, image := settings(t)
	ctx := context.Background()
	quiet := func(string) {}
	source, cleanup := emptySandbox(t, image)
	defer cleanup()
	seed, err := conn.Open(ctx, source.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close(ctx)
	if _, err := seed.Exec(ctx, `
		create extension btree_gist;
		create table kept (id int primary key);
		insert into kept select generate_series(1, 3);
		create table "Empty one" (id int);
		create table parted (id int) partition by range (id);
		create table parted_low partition of parted for values from (1) to (10);
		insert into parted values (5)`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	s, err := conn.SettingsFor(source.DSN())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, BackupName("sandbox", image, time.Now()))
	if err := Backup(ctx, s, image, dest, quiet); err != nil {
		t.Fatal(err)
	}

	before, _ := Sandboxes(ctx)
	report, err := Verify(ctx, image, dest, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Problem(); got != "" {
		t.Errorf("problem: %s (%+v)", got, report)
	}
	if report.ListedTables != 4 || report.Rows() != 4 {
		t.Errorf("report = %+v", report)
	}
	if report.Counts["public.kept"] != 3 || report.Counts[`public."Empty one"`] != 0 {
		t.Errorf("counts = %+v", report.Counts)
	}

	// Compared with its source, nothing differs until the source changes.
	live, err := TableCounts(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if got := Differences(report.Counts, live); len(got) != 0 {
		t.Errorf("differences = %q", got)
	}
	if _, err := seed.Exec(ctx, "insert into kept values (4)").ReadAll(); err != nil {
		t.Fatal(err)
	}
	live, _ = TableCounts(ctx, seed)
	if got := Differences(report.Counts, live); len(got) != 1 {
		t.Errorf("after a change: %q", got)
	}

	// A backup cut short must fail, not pass with what it has.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.backup")
	if err := os.WriteFile(broken, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	cut, err := Verify(ctx, image, broken, quiet)
	if err == nil && cut.Problem() == "" {
		t.Errorf("a truncated backup passed: %+v", cut)
	}

	after, _ := Sandboxes(ctx)
	if len(after) != len(before) {
		t.Errorf("sandbox left running: %+v", after)
	}
}

func TestRestoreImage(t *testing.T) {
	for _, tt := range []struct{ name, server, want, wantErr string }{
		{"a-pg17-20261105-100000.backup", "postgres:17", "postgres:17", ""},
		{"a-pg16-20261105-100000.backup", "postgres:17", "postgres:17", ""},
		{"a-pg17-20261105-100000.backup", "postgres:16", "", "from Postgres 17"},
		{"other_app.backup", "postgres:15", "postgres:latest", ""},
	} {
		got, err := restoreImage(tt.name, tt.server)
		if got != tt.want || (tt.wantErr == "") != (err == nil) {
			t.Errorf("%s on %s: got %q, %v", tt.name, tt.server, got, err)
		}
	}
}

func TestRestoreName(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for backup, want := range map[string]string{
		"crm_db_training-pg17-20261105-100000.backup": "crm_db_training_restored_20261106",
		"shop-db-next-pg17-20261105-100000.backup":    "shop_db_next_restored_20261106",
		"other_app.backup":                            "other_app_restored_20261106",
		"2026 dump.backup":                            "db_2026_dump_restored_20261106",
	} {
		got := RestoreName(backup, at)
		if got != want {
			t.Errorf("%s: got %q", backup, got)
		}
		if !ValidDatabase(got) {
			t.Errorf("%q isn't valid", got)
		}
	}
	long := RestoreName(strings.Repeat("x", 100)+".backup", at)
	if !ValidDatabase(long) {
		t.Errorf("long name invalid: %q (%d)", long, len(long))
	}
}

func TestValidDatabase(t *testing.T) {
	for name, want := range map[string]bool{
		"ok_name": true, "_x": true, "Mixed1": true,
		"": false, "1x": false, "a-b": false, "a b": false, `a"; drop database x; --`: false, strings.Repeat("x", 64): false,
	} {
		if got := ValidDatabase(name); got != want {
			t.Errorf("%q: got %v", name, got)
		}
	}
}

func databases(t *testing.T, sb Sandbox) []string {
	t.Helper()
	ctx := context.Background()
	c, err := conn.Open(ctx, sb.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	res, err := c.Exec(ctx, "select datname from pg_database where not datistemplate order by 1").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range res[0].Rows {
		names = append(names, string(row[0]))
	}
	return names
}

func TestRestore(t *testing.T) {
	_, image := settings(t)
	ctx := context.Background()
	quiet := func(string) {}

	source, cleanup := namedSandbox(t, image, "test-restore-source")
	defer cleanup()
	seed, err := conn.Open(ctx, source.DSN())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, "create table kept (id int primary key); insert into kept select generate_series(1, 3)").ReadAll(); err != nil {
		t.Fatal(err)
	}
	seed.Close(ctx)
	s, err := conn.SettingsFor(source.DSN())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, BackupName("sandbox", image, time.Now()))
	if err := Backup(ctx, s, image, dest, quiet); err != nil {
		t.Fatal(err)
	}

	server, cleanupServer := namedSandbox(t, image, "test-restore-target")
	defer cleanupServer()
	ts, err := conn.SettingsFor(server.DSN())
	if err != nil {
		t.Fatal(err)
	}
	target := Target{DSN: server.DSN(), Settings: ts, Image: image}

	// An existing database with data in it, which nothing below may touch.
	existing := conn.WithDatabase(server.DSN(), "precious")
	admin, err := conn.Open(ctx, server.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create database precious").ReadAll(); err != nil {
		t.Fatal(err)
	}
	p, err := conn.Open(ctx, existing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "create table mine (id int); insert into mine values (42)").ReadAll(); err != nil {
		t.Fatal(err)
	}
	p.Close(ctx)
	preciousIntact := func(when string) {
		t.Helper()
		c, err := conn.Open(ctx, existing)
		if err != nil {
			t.Fatalf("%s: precious is gone: %v", when, err)
		}
		defer c.Close(ctx)
		res, err := c.Exec(ctx, "select id from mine").ReadAll()
		if err != nil || string(res[0].Rows[0][0]) != "42" {
			t.Fatalf("%s: precious changed: %v", when, err)
		}
	}

	report, err := Restore(ctx, target, dest, "fresh", quiet)
	if err != nil {
		t.Fatal(err)
	}
	if report.ListedTables != 1 || report.Rows() != 3 {
		t.Errorf("report = %+v", report)
	}
	c, err := conn.Open(ctx, conn.WithDatabase(server.DSN(), "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Exec(ctx, "select count(*), current_database() from kept").ReadAll()
	c.Close(ctx)
	if err != nil || string(res[0].Rows[0][0]) != "3" || string(res[0].Rows[0][1]) != "fresh" {
		t.Errorf("restored: %v, %v", res, err)
	}

	// A name that's taken stops everything, and the database that holds it stays.
	if _, err := Restore(ctx, target, dest, "precious", quiet); err == nil {
		t.Fatal("a taken name: want an error")
	}
	preciousIntact("after a taken name")

	// A bad backup leaves nothing behind.
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.backup")
	if err := os.WriteFile(broken, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, target, broken, "halfway", quiet); err == nil {
		t.Fatal("a broken backup: want an error")
	}

	// Stopping leaves nothing behind either.
	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := Restore(stopped, target, dest, "stopped", func(line string) {
		if strings.HasPrefix(line, "Restoring") {
			cancel()
		}
	}); err == nil {
		t.Fatal("stopped: want an error")
	}

	if _, err := Restore(ctx, target, dest, `bad"; drop database precious; --`, quiet); err == nil {
		t.Fatal("a bad name: want an error")
	}
	preciousIntact("at the end")
	if got := databases(t, server); !slices.Equal(got, []string{"fresh", "postgres", "precious", "sandbox"}) {
		t.Errorf("databases = %v", got)
	}
}

// emptySandbox starts a sandbox with nothing restored, to act as a source.
func emptySandbox(t *testing.T, image string) (Sandbox, func()) {
	return namedSandbox(t, image, "test-source")
}

func namedSandbox(t *testing.T, image, name string) (Sandbox, func()) {
	t.Helper()
	ctx := context.Background()
	sb := Sandbox{Name: sandboxPrefix + name, Password: "test", Source: "none"}
	_ = Remove(ctx, sb.Name)
	if _, err := output(ctx, []string{"POSTGRES_PASSWORD=test", "POSTGRES_DB=" + sandboxDB},
		"run", "--detach", "--rm", "--name", sb.Name, "--label", sandboxLabel,
		"-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB", "--publish", "127.0.0.1::5432", image); err != nil {
		t.Fatal(err)
	}
	if err := waitUntilReady(ctx, sb.Name); err != nil {
		t.Fatal(err)
	}
	out, err := output(ctx, nil, "port", sb.Name, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Port, err = portOf(out); err != nil {
		t.Fatal(err)
	}
	return sb, func() { _ = Remove(ctx, sb.Name) }
}
