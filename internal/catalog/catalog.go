// Package catalog reads the object tree of the connected database from the
// system catalogs, one level at a time.
//
// It has its own connection because a session's is tied up while a result is
// being paged, and running anything else on it would cancel that result.
package catalog

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

// RootKind is the kind of the tree's top level.
const RootKind = "schemas"

// Node is one entry in the tree.
type Node struct {
	Label  string
	Detail string // shown beside the label, e.g. a column's type
	SQL    string // a query for this object, if it has rows to show
	Kind   string // the kind to pass to Children to expand it; empty for a leaf
	ID     string // the id to pass to Children
}

// groups are the fixed folders under a schema or relation. Their children use
// the same id as the parent.
var groups = map[string][]Node{
	"schema": {
		{Label: "Tables", Kind: "tables"},
		{Label: "Views", Kind: "views"},
		{Label: "Materialized views", Kind: "matviews"},
		{Label: "Functions", Kind: "functions"},
		{Label: "Sequences", Kind: "sequences"},
	},
	"table": {
		{Label: "Columns", Kind: "columns"},
		{Label: "Indexes", Kind: "indexes"},
		{Label: "Constraints", Kind: "constraints"},
	},
	"view": {
		{Label: "Columns", Kind: "columns"},
	},
	"matview": {
		{Label: "Columns", Kind: "columns"},
		{Label: "Indexes", Kind: "indexes"},
	},
}

// relations lists a schema's relations of the given relkinds, which expand as
// the given kind. Postgres does the identifier quoting, via format's %I.
func relations(relkinds, kind string) string {
	return fmt.Sprintf(`
		select c.relname, '', format('select * from %%I.%%I limit 100;', n.nspname, c.relname), '%s', c.oid::text
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = $1 and c.relkind in (%s)
		order by 1`, kind, relkinds)
}

// queries return one row per node: label, detail, sql, kind, id. A kind's id,
// if it has one, is bound as $1: a schema name or a relation's oid.
var queries = map[string]string{
	// Hides pg_catalog, pg_toast and the like, as pgAdmin does by default.
	RootKind: `
		select nspname, '', '', 'schema', nspname
		from pg_namespace
		where nspname !~ '^pg_' and nspname <> 'information_schema'
		order by 1`,
	"tables":    relations(`'r', 'p'`, "table"),
	"views":     relations(`'v'`, "view"),
	"matviews":  relations(`'m'`, "matview"),
	"sequences": relations(`'S'`, ""),
	"functions": `
		select p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
			pg_get_function_result(p.oid), '', '', ''
		from pg_proc p
		join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = $1 and p.prokind in ('f', 'p')
		order by 1`,
	"columns": `
		select attname,
			format_type(atttypid, atttypmod) || case when attnotnull then ' not null' else '' end,
			'', '', ''
		from pg_attribute
		where attrelid = $1 and attnum > 0 and not attisdropped
		order by attnum`,
	"indexes": `
		select c.relname, pg_get_indexdef(i.indexrelid), '', '', ''
		from pg_index i
		join pg_class c on c.oid = i.indexrelid
		where i.indrelid = $1
		order by 1`,
	"constraints": `
		select conname, pg_get_constraintdef(oid), '', '', ''
		from pg_constraint
		where conrelid = $1
		order by 1`,
}

// Catalog reads the tree over one connection, opened on first use and
// reopened if lost.
type Catalog struct {
	connect func(context.Context) (*pgconn.PgConn, error)

	mu   sync.Mutex
	conn *pgconn.PgConn // guarded by mu
}

// New returns a catalog that connects with connect.
func New(connect func(context.Context) (*pgconn.PgConn, error)) *Catalog {
	return &Catalog{connect: connect}
}

// Children returns the nodes under a node of the given kind and id. Start from
// RootKind, which has no id.
func (c *Catalog) Children(ctx context.Context, kind, id string) ([]Node, error) {
	if g, ok := groups[kind]; ok {
		nodes := make([]Node, len(g))
		for i, n := range g {
			n.ID = id
			nodes[i] = n
		}
		return nodes, nil
	}
	sql, ok := queries[kind]
	if !ok {
		return nil, fmt.Errorf("unknown tree node kind %q", kind)
	}
	var params [][]byte
	if id != "" {
		params = [][]byte{[]byte(id)}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil || c.conn.IsClosed() {
		conn, err := c.connect(ctx)
		if err != nil {
			return nil, err
		}
		c.conn = conn
	}
	res := c.conn.ExecParams(ctx, sql, params, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	nodes := make([]Node, len(res.Rows))
	for i, r := range res.Rows {
		nodes[i] = Node{
			Label:  string(r[0]),
			Detail: string(r[1]),
			SQL:    string(r[2]),
			Kind:   string(r[3]),
			ID:     string(r[4]),
		}
	}
	return nodes, nil
}

// Close closes the connection, if one was opened.
func (c *Catalog) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.Close(ctx)
}
