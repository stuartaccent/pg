package catalog

import (
	"context"
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
		create temp table "Odd ""name" (id int primary key, label varchar(10));
		create temp view odd_view as select 1 as one;
		create temp sequence odd_seq;
		select pg_my_temp_schema()::regnamespace::text`
	res, err := c.conn.Exec(context.Background(), setup).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	schema := string(res[len(res)-1].Rows[0][0])

	if got := labels(children(t, c, "schema", schema)); !slices.Equal(got, []string{"Tables", "Views", "Materialized views", "Functions", "Sequences"}) {
		t.Errorf("schema groups = %v", got)
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
	if got := labels(cols); !slices.Equal(got, []string{"id", "label"}) {
		t.Errorf("columns = %v", got)
	}
	if cols[0].Detail != "integer not null" || cols[1].Detail != "character varying(10)" {
		t.Errorf("column details = %q, %q", cols[0].Detail, cols[1].Detail)
	}
	if idx := children(t, c, "indexes", table.ID); len(idx) != 1 || !strings.Contains(idx[0].Detail, "UNIQUE INDEX") {
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
