package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

// These tests need a real server. Set PGIDE_TEST_SERVICE to a service name
// from your pg_service.conf; they only run read-only statements, temp objects
// and work that's rolled back.
func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	c := New(func(ctx context.Context) (*pgconn.PgConn, error) { return conn.Open(ctx, conn.ServiceDSN(service)) })
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func children(t *testing.T, c *Catalog, kind, id string) []Node {
	t.Helper()
	nodes, err := c.Children(context.Background(), kind, id)
	if err != nil {
		t.Fatalf("Children(%q, %q): %v", kind, id, err)
	}
	return nodes
}

func labels(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label
	}
	return out
}

func TestRootHidesSystemSchemas(t *testing.T) {
	c := newTestCatalog(t)
	root := children(t, c, RootKind, "")
	if last := root[len(root)-1]; last.Label != "Roles" || last.Kind != "roles" {
		t.Errorf("last node = %+v, want Roles", last)
	}
	for _, n := range root[:len(root)-1] {
		if strings.HasPrefix(n.Label, "pg_") || n.Label == "information_schema" {
			t.Errorf("system schema %q listed", n.Label)
		}
		if n.Kind != "schema" || n.ID != n.Label {
			t.Errorf("schema node = %+v", n)
		}
	}
}

func TestWalk(t *testing.T) {
	c := newTestCatalog(t)
	// Temp objects live on the catalog's own connection, so opening it is the
	// first step. The awkward name proves identifiers get quoted.
	children(t, c, RootKind, "")
	setup := `
		create temp table "Odd ""name" (id int primary key, label varchar(10) default 'x', extra int generated always as (id * 2) stored);
		create index on "Odd ""name" (label);
		create trigger odd_trigger before update on "Odd ""name" for each row execute function suppress_redundant_updates_trigger();
		create temp table parted (id int) partition by range (id);
		create temp table parted_low partition of parted for values from (1) to (10);
		create temp view odd_view as select 1 as one;
		create function pg_temp.odd_function() returns int language sql as 'select 1';
		create function pg_temp.odd_trigger_function() returns trigger language plpgsql as 'begin return new; end';
		create temp sequence odd_seq;
		select pg_my_temp_schema()::regnamespace::text`
	res, err := c.conn.Exec(context.Background(), setup).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	schema := string(res[len(res)-1].Rows[0][0])

	schemaGroups := children(t, c, "schema", schema)
	if got := labels(schemaGroups); !slices.Equal(got, []string{"Tables", "Views", "Materialized views", "Functions", "Trigger functions", "Sequences", "Collations", "Domains", "Types"}) {
		t.Errorf("schema groups = %v", got)
	}
	if schemaGroups[0].Detail != "2" || schemaGroups[3].Detail != "1" || schemaGroups[4].Detail != "1" || schemaGroups[0].ID != schema {
		t.Errorf("schema group details = %+v", schemaGroups)
	}

	tables := children(t, c, "tables", schema)
	if got := labels(tables); !slices.Equal(got, []string{`Odd "name`, "parted"}) {
		t.Fatalf("tables = %v", got)
	}
	table, parted := tables[0], tables[1]
	if table.Kind != "table" || parted.Kind != "partitioned" {
		t.Errorf("table kinds = %q, %q", table.Kind, parted.Kind)
	}
	if got := labels(children(t, c, parted.Kind, parted.ID)); got[0] != "Partitions" {
		t.Errorf("partitioned groups = %v", got)
	}
	if got := children(t, c, "partitions", parted.ID); len(got) != 1 || got[0].Label != "parted_low" || got[0].Kind != "table" {
		t.Errorf("partitions = %+v", got)
	}
	if want := `"Odd ""name" limit 100;`; !strings.HasSuffix(table.SQL, want) {
		t.Errorf("SQL = %q, want suffix %q", table.SQL, want)
	}
	if rows, err := c.conn.Exec(context.Background(), table.SQL).ReadAll(); err != nil {
		t.Errorf("generated SQL failed: %v (%v)", err, rows)
	}

	groups := children(t, c, table.Kind, table.ID)
	if got := labels(groups); !slices.Equal(got, []string{"Columns", "Indexes", "Constraints", "Triggers"}) {
		t.Errorf("table groups = %v", got)
	}
	if groups[0].ID != table.ID {
		t.Errorf("group id = %q, want %q", groups[0].ID, table.ID)
	}

	cols := children(t, c, "columns", table.ID)
	if got := labels(cols); !slices.Equal(got, []string{"id", "label", "extra"}) {
		t.Errorf("columns = %v", got)
	}
	if cols[0].Detail != "integer not null" || cols[1].Detail != "character varying(10)" {
		t.Errorf("column details = %q, %q", cols[0].Detail, cols[1].Detail)
	}
	if idx := children(t, c, "indexes", table.ID); len(idx) != 2 {
		t.Errorf("indexes = %+v", idx)
	}
	if con := children(t, c, "constraints", table.ID); len(con) != 1 || con[0].Detail != "PRIMARY KEY (id)" {
		t.Errorf("constraints = %+v", con)
	}

	if trg := children(t, c, "triggers", table.ID); len(trg) != 1 || trg[0].Label != "odd_trigger" {
		t.Errorf("triggers = %+v", trg)
	}

	// The estimate appears once the table has been analyzed.
	if _, err := c.conn.Exec(context.Background(), `insert into "Odd ""name" (id) values (1); analyze "Odd ""name"`).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if got := children(t, c, "tables", schema)[0].Detail; got != "~1" {
		t.Errorf("row estimate = %q", got)
	}

	if v := children(t, c, "views", schema); len(v) != 1 || v[0].Kind != "view" {
		t.Errorf("views = %+v", v)
	}
	if s := children(t, c, "sequences", schema); len(s) != 1 || s[0].Kind != "" || s[0].SQL == "" {
		t.Errorf("sequences = %+v", s)
	}
	if f := children(t, c, "functions", schema); len(f) != 1 || f[0].Label != "odd_function()" || f[0].Detail != "integer" {
		t.Errorf("functions = %+v", f)
	}
	f := children(t, c, "triggerfunctions", schema)
	if len(f) != 1 || f[0].Label != "odd_trigger_function()" || f[0].Detail != "" {
		t.Fatalf("trigger functions = %+v", f)
	}
	if def, err := c.Definition(context.Background(), f[0].Def, f[0].ID); err != nil || !strings.Contains(def, "RETURNS trigger") {
		t.Errorf("trigger function definition = %q, %v", def, err)
	}

	// Each definition must be SQL that Postgres accepts. Run them in a rolled
	// back transaction, under a new schema name so nothing clashes.
	ctx := context.Background()
	def, err := c.Definition(ctx, table.Def, table.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id integer not null", "default 'x'", "generated always as ((id * 2)) stored", "PRIMARY KEY (id)", "CREATE INDEX", "CREATE TRIGGER odd_trigger"} {
		if !strings.Contains(def, want) {
			t.Errorf("definition lacks %q:\n%s", want, def)
		}
	}
	rerun := strings.NewReplacer(schema+".", "defcheck.", `"Odd ""name_pkey"`, "defcheck_pkey",
		`"Odd ""name_label_idx" ON pg_temp.`, "defcheck_idx ON defcheck.",
		`ON pg_temp."Odd ""name"`, `ON defcheck."Odd ""name"`).Replace(def)
	if _, err := c.conn.Exec(ctx, "begin; create schema defcheck; "+rerun).ReadAll(); err != nil {
		t.Errorf("table definition does not run: %v\n%s", err, rerun)
	}
	if _, err := c.conn.Exec(ctx, "rollback").ReadAll(); err != nil {
		t.Fatal(err)
	}

	view := children(t, c, "views", schema)[0]
	if def, err := c.Definition(ctx, view.Def, view.ID); err != nil || !strings.Contains(def, "create or replace view") {
		t.Errorf("view definition = %q, %v", def, err)
	}
	seq := children(t, c, "sequences", schema)[0]
	if def, err := c.Definition(ctx, seq.Def, seq.ID); err != nil || !strings.Contains(def, "create sequence") {
		t.Errorf("sequence definition = %q, %v", def, err)
	}
	if _, err := c.Definition(ctx, "nope", "1"); err == nil {
		t.Error("unknown definition: want an error")
	}
	if _, err := c.Definition(ctx, "view", "0"); err == nil {
		t.Error("missing object: want an error")
	}
}

func TestCollationsDomainsAndTypes(t *testing.T) {
	c := newTestCatalog(t)
	children(t, c, RootKind, "") // opens the connection the temp objects live on
	ctx := context.Background()
	setup := `
		create temp table odd_table (id int);
		create type pg_temp."Odd enum" as enum ('a', 'it''s');
		create type pg_temp.odd_composite as (id int, label varchar(10));
		create type pg_temp.odd_range as range (subtype = int4);
		create domain pg_temp.odd_domain as int default 1 not null constraint positive check (value > 0);
		create collation pg_temp.odd_collation (provider = libc, locale = 'C');
		select pg_my_temp_schema()::regnamespace::text`
	res, err := c.conn.Exec(ctx, setup).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	schema := string(res[len(res)-1].Rows[0][0])

	groups := children(t, c, "schema", schema)
	details := map[string]string{}
	for _, g := range groups {
		details[g.Label] = g.Detail
	}
	if details["Collations"] != "1" || details["Domains"] != "1" || details["Types"] != "3" {
		t.Errorf("group counts = %v", details)
	}

	// Just these: no arrays, no multirange, no row type for the table, no domain.
	types := children(t, c, "types", schema)
	if got := labels(types); !slices.Equal(got, []string{"Odd enum", "odd_composite", "odd_range"}) {
		t.Fatalf("types = %v", got)
	}
	if types[0].Detail != "enum" || types[1].Detail != "composite" || types[2].Detail != "range" {
		t.Errorf("type details = %+v", types)
	}
	domains := children(t, c, "domains", schema)
	if len(domains) != 1 || domains[0].Detail != "integer" {
		t.Fatalf("domains = %+v", domains)
	}
	collations := children(t, c, "collations", schema)
	if len(collations) != 1 || collations[0].Detail != "libc" {
		t.Fatalf("collations = %+v", collations)
	}

	// Each definition must be SQL that Postgres accepts.
	nodes := append(append(types, domains...), collations...)
	var script strings.Builder
	script.WriteString("begin; create schema defcheck; ")
	for _, n := range nodes {
		def, err := c.Definition(ctx, n.Def, n.ID)
		if err != nil {
			t.Fatalf("%s: %v", n.Label, err)
		}
		script.WriteString(strings.ReplaceAll(def, schema+".", "defcheck.") + "\n")
	}
	if _, err := c.conn.Exec(ctx, script.String()).ReadAll(); err != nil {
		t.Errorf("definitions don't run: %v\n%s", err, script.String())
	}
	if _, err := c.conn.Exec(ctx, "rollback").ReadAll(); err != nil {
		t.Fatal(err)
	}

	def, err := c.Definition(ctx, domains[0].Def, domains[0].ID)
	for _, want := range []string{"default 1", "not null", "constraint positive CHECK"} {
		if err != nil || !strings.Contains(def, want) {
			t.Errorf("domain definition lacks %q: %q, %v", want, def, err)
		}
	}
	if def, err := c.Definition(ctx, types[0].Def, types[0].ID); err != nil || !strings.Contains(def, `'it''s'`) {
		t.Errorf("enum definition = %q, %v", def, err)
	}

	// A base type can't be created here, but its definition can be read.
	int4, err := c.conn.Exec(ctx, "select 'int4'::regtype::oid").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if def, err := c.Definition(ctx, "type", string(int4[0].Rows[0][0])); err != nil || !strings.Contains(def, "input = int4in") || !strings.Contains(def, "internallength = 4") {
		t.Errorf("base type definition = %q, %v", def, err)
	}
}

func TestICUCollationDefinition(t *testing.T) {
	c := newTestCatalog(t)
	children(t, c, RootKind, "") // opens the connection the temp objects live on
	ctx := context.Background()
	res, err := c.conn.Exec(ctx, `
		create collation pg_temp.odd_icu (provider = icu, locale = 'und', deterministic = false);
		select pg_my_temp_schema()::regnamespace::text`).ReadAll()
	if err != nil {
		t.Skipf("server has no ICU: %v", err)
	}
	schema := string(res[len(res)-1].Rows[0][0])
	node := children(t, c, "collations", schema)[0]
	if node.Detail != "icu" {
		t.Errorf("provider = %q", node.Detail)
	}
	def, err := c.Definition(ctx, node.Def, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider = icu", "locale = 'und'", "deterministic = false"} {
		if !strings.Contains(def, want) {
			t.Errorf("definition lacks %q: %s", want, def)
		}
	}
	rerun := "begin; create schema defcheck; " + strings.ReplaceAll(def, schema+".", "defcheck.")
	if _, err := c.conn.Exec(ctx, rerun).ReadAll(); err != nil {
		t.Errorf("definition doesn't run: %v\n%s", err, def)
	}
	if _, err := c.conn.Exec(ctx, "rollback").ReadAll(); err != nil {
		t.Fatal(err)
	}
}

func TestSearch(t *testing.T) {
	c := newTestCatalog(t)
	if got, want := labels(children(t, c, SearchKind, "")), labels(children(t, c, RootKind, "")); !slices.Equal(got, want) {
		t.Errorf("empty search = %v, want the top level %v", got, want)
	}
	// Temp schemas are hidden from search, like every pg_ schema.
	if _, err := c.conn.Exec(context.Background(), "create temp table searchable_tmp (id int)").ReadAll(); err != nil {
		t.Fatal(err)
	}
	if got := children(t, c, SearchKind, "searchable_tmp"); len(got) != 0 {
		t.Errorf("search found a system schema's table: %+v", got)
	}
	// % must be a literal character, not a wildcard.
	if got := children(t, c, SearchKind, "%"); len(got) != 0 {
		t.Errorf("search for %% = %d nodes", len(got))
	}
}

func TestClosedCatalogDoesNotReconnect(t *testing.T) {
	c := newTestCatalog(t)
	children(t, c, RootKind, "")
	connects := 0
	connect := c.connect
	c.connect = func(ctx context.Context) (*pgconn.PgConn, error) {
		connects++
		return connect(ctx)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Children(context.Background(), RootKind, ""); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	if connects != 0 {
		t.Errorf("reconnected %d times", connects)
	}
}

func TestCompletions(t *testing.T) {
	c := newTestCatalog(t)
	got, err := c.Completions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Schemas) > 0 && got.DefaultSchema == "" {
		t.Errorf("no default schema: %+v", got.DefaultSchema)
	}
}

func TestTable(t *testing.T) {
	c := newTestCatalog(t)
	ctx := context.Background()
	for _, sql := range []string{
		`create temp table "Two key" (a int, dropped int, "B" text, c text, primary key ("B", a))`,
		`alter table "Two key" drop column dropped`,
		`create temp table nokey (a int)`,
	} {
		if _, err := c.read(ctx, sql, nil); err != nil {
			t.Fatal(err)
		}
	}
	oids, err := c.read(ctx, `select 'pg_temp."Two key"'::regclass::oid, 'pg_temp.nokey'::regclass::oid`, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.Table(ctx, string(oids[0][0]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got.Name, `."Two key"`) || !strings.Contains(string(got.Columns), `"3" : "\"B\""`) ||
		strings.Contains(string(got.Columns), `"2"`) || string(got.Key) != "[3, 1]" {
		t.Errorf("table = %s %s %s", got.Name, got.Columns, got.Key)
	}
	if got, err := c.Table(ctx, string(oids[0][1])); err != nil || string(got.Key) != "[]" {
		t.Errorf("no key = %s, %v", got.Key, err)
	}
	if _, err := c.Table(ctx, "0"); err == nil {
		t.Error("expected an error for a missing table")
	}
}

func TestBadInput(t *testing.T) {
	c := newTestCatalog(t)
	if _, err := c.Children(context.Background(), "nope", ""); err == nil {
		t.Error("unknown kind: want an error")
	}
	// A bad id is a bound parameter: the server rejects it, nothing runs.
	if _, err := c.Children(context.Background(), "columns", "1; drop table x"); err == nil {
		t.Error("bad oid: want an error")
	}
	// The connection must survive that error.
	children(t, c, RootKind, "")
}

func TestReconnectsWithoutFailing(t *testing.T) {
	c := newTestCatalog(t)
	ctx := context.Background()
	children(t, c, RootKind, "")
	pid := c.conn.PID()
	other, err := c.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, fmt.Sprintf("select pg_terminate_backend(%d)", pid)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	children(t, c, RootKind, "")
}

// Roles can't be temporary, so these only exist inside a transaction.
func TestRoles(t *testing.T) {
	c := newTestCatalog(t)
	children(t, c, RootKind, "") // opens the connection
	exec := func(sql string) error {
		_, err := c.conn.Exec(context.Background(), sql).ReadAll()
		return err
	}
	if err := exec("begin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec("rollback") })
	if err := exec(`
		create role pgide_group noinherit;
		create role pgide_member login connection limit 3 password 'pgide-secret';
		grant pgide_group to pgide_member with admin option;
		alter role pgide_member set work_mem to '64MB';
		grant usage on schema public to pgide_member`); err != nil {
		t.Skipf("can't create roles here: %v", err)
	}

	byLabel := map[string]Node{}
	for _, n := range children(t, c, "roles", "") {
		if strings.HasPrefix(n.Label, "pg_") {
			t.Errorf("built-in role %q listed", n.Label)
		}
		byLabel[n.Label] = n
	}
	member, group := byLabel["pgide_member"], byLabel["pgide_group"]
	if member.Detail != "login" || member.Kind != "role" || member.Def != "role" {
		t.Errorf("member node = %+v", member)
	}
	if got := labels(children(t, c, "role", member.ID)); !slices.Equal(got, []string{"Member of", "Members", "Grants in this database"}) {
		t.Errorf("role groups = %q", got)
	}
	if got := children(t, c, "memberof", member.ID); len(got) != 1 || got[0].Label != "pgide_group" || got[0].Detail != "with admin option" {
		t.Errorf("member of = %+v", got)
	}
	if got := children(t, c, "members", group.ID); len(got) != 1 || got[0].Label != "pgide_member" {
		t.Errorf("members = %+v", got)
	}
	if got := children(t, c, "grants", member.ID); len(got) != 1 || got[0].Label != "schema public" || got[0].Detail != "usage" {
		t.Errorf("grants = %+v", got)
	}
	if got := children(t, c, SearchKind, "pgide_mem"); len(got) != 1 || got[0].Def != "role" {
		t.Errorf("search = %+v", got)
	}

	def, err := c.Definition(context.Background(), "role", member.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"create role pgide_member login connection limit 3;",
		"alter role pgide_member set work_mem to '64MB';",
		"grant pgide_group to pgide_member with admin option;",
	} {
		if !strings.Contains(def, want) {
			t.Errorf("definition lacks %q:\n%s", want, def)
		}
	}
	if strings.Contains(def, "pgide-secret") || strings.Contains(def, "SCRAM") {
		t.Errorf("definition shows the password:\n%s", def)
	}
}

func TestActivityShowsBlockingAndStops(t *testing.T) {
	c := newTestCatalog(t)
	ctx := context.Background()
	open := func() *pgconn.PgConn {
		other, err := conn.Open(ctx, conn.ServiceDSN(os.Getenv("PGIDE_TEST_SERVICE")))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = other.Close(ctx) })
		return other
	}
	const lock = "select pg_advisory_lock(7306115)"
	holder, waiter := open(), open()
	if _, err := holder.Exec(ctx, lock).ReadAll(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(ctx, lock).ReadAll()
		waited <- err
	}()

	find := func(pid uint32) (Backend, bool) {
		backends, err := c.Activity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(backends, func(b Backend) bool { return b.PID == int(pid) })
		if i < 0 {
			return Backend{}, false
		}
		return backends[i], true
	}
	var blocked Backend
	for deadline := time.Now().Add(5 * time.Second); blocked.BlockedBy == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		blocked, _ = find(waiter.PID())
	}
	if blocked.BlockedBy != fmt.Sprint(holder.PID()) || blocked.State != "active" || blocked.Query != lock || blocked.App != "pg" || blocked.For == "" {
		t.Fatalf("blocked = %+v", blocked)
	}

	if err := c.Stop(ctx, int(waiter.PID()), false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err == nil || !strings.Contains(err.Error(), "canceling") {
			t.Errorf("cancelled query: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel didn't stop the query")
	}
	if _, ok := find(waiter.PID()); !ok {
		t.Error("cancelling should leave the connection")
	}

	if err := c.Stop(ctx, int(waiter.PID()), true); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok := find(waiter.PID()); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminate didn't end the connection")
		}
	}
	if err := c.Stop(ctx, 1, false); err == nil {
		t.Error("stopping a pid that isn't a connection: want an error")
	}
}
