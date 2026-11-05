package catalog

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

// These tests need a real server. Set PGIDE_TEST_SERVICE to a service name
// from your pg_service.conf; they only run read-only statements and temp objects.
func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	c := New(func(ctx context.Context) (*pgconn.PgConn, error) { return conn.Connect(ctx, service) })
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
	for _, n := range children(t, c, RootKind, "") {
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
		create temp view odd_view as select 1 as one;
		create temp sequence odd_seq;
		select pg_my_temp_schema()::regnamespace::text`
	res, err := c.conn.Exec(context.Background(), setup).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	schema := string(res[len(res)-1].Rows[0][0])

	schemaGroups := children(t, c, "schema", schema)
	if got := labels(schemaGroups); !slices.Equal(got, []string{"Tables", "Views", "Materialized views", "Functions", "Sequences"}) {
		t.Errorf("schema groups = %v", got)
	}
	if schemaGroups[0].Detail != "1" || schemaGroups[3].Detail != "0" || schemaGroups[0].ID != schema {
		t.Errorf("schema group details = %+v", schemaGroups)
	}

	tables := children(t, c, "tables", schema)
	if len(tables) != 1 || tables[0].Label != `Odd "name` || tables[0].Kind != "table" {
		t.Fatalf("tables = %+v", tables)
	}
	table := tables[0]
	if want := `"Odd ""name" limit 100;`; !strings.HasSuffix(table.SQL, want) {
		t.Errorf("SQL = %q, want suffix %q", table.SQL, want)
	}
	if rows, err := c.conn.Exec(context.Background(), table.SQL).ReadAll(); err != nil {
		t.Errorf("generated SQL failed: %v (%v)", err, rows)
	}

	groups := children(t, c, table.Kind, table.ID)
	if got := labels(groups); !slices.Equal(got, []string{"Columns", "Indexes", "Constraints"}) {
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

	if v := children(t, c, "views", schema); len(v) != 1 || v[0].Kind != "view" {
		t.Errorf("views = %+v", v)
	}
	if s := children(t, c, "sequences", schema); len(s) != 1 || s[0].Kind != "" || s[0].SQL == "" {
		t.Errorf("sequences = %+v", s)
	}
	if f := children(t, c, "functions", schema); len(f) != 0 {
		t.Errorf("functions = %+v", f)
	}

	// Each definition must be SQL that Postgres accepts. Run them in a rolled
	// back transaction, under a new schema name so nothing clashes.
	ctx := context.Background()
	def, err := c.Definition(ctx, table.Def, table.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id integer not null", "default 'x'", "generated always as ((id * 2)) stored", "PRIMARY KEY (id)", "CREATE INDEX"} {
		if !strings.Contains(def, want) {
			t.Errorf("definition lacks %q:\n%s", want, def)
		}
	}
	rerun := strings.NewReplacer(schema+".", "defcheck.", `"Odd ""name_pkey"`, "defcheck_pkey",
		`"Odd ""name_label_idx" ON pg_temp.`, "defcheck_idx ON defcheck.").Replace(def)
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
	if _, err := c.Completions(context.Background()); err != nil {
		t.Fatal(err)
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
