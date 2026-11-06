// Package catalog reads the object tree of the connected database from the
// system catalogs, one level at a time, and what's happening on its server.
//
// It has its own connection because a session's is tied up while a result is
// being paged, and running anything else on it would cancel that result.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

const (
	// RootKind is the kind of the tree's top level.
	RootKind = "schemas"
	// SearchKind finds objects by name. Its id is the term; an empty one gives
	// the top level.
	SearchKind = "search"
)

// ErrClosed is returned once the catalog has been closed.
var ErrClosed = errors.New("catalog is closed")

// Node is one entry in the tree.
type Node struct {
	Label  string
	Detail string // shown beside the label, e.g. a column's type
	SQL    string // a query for this object, if it has rows to show
	Kind   string // the kind to pass to Children to expand it; empty for a leaf
	ID     string // the id to pass to Children and Definition
	Def    string // the def to pass to Definition; empty if it has none
}

// groups are the fixed folders under a relation. Their children use the same
// id as the parent.
var groups = map[string][]Node{
	"table": {
		{Label: "Columns", Kind: "columns"},
		{Label: "Indexes", Kind: "indexes"},
		{Label: "Constraints", Kind: "constraints"},
		{Label: "Triggers", Kind: "triggers"},
	},
	"partitioned": {
		{Label: "Partitions", Kind: "partitions"},
		{Label: "Columns", Kind: "columns"},
		{Label: "Indexes", Kind: "indexes"},
		{Label: "Constraints", Kind: "constraints"},
		{Label: "Triggers", Kind: "triggers"},
	},
	"view": {
		{Label: "Columns", Kind: "columns"},
	},
	"matview": {
		{Label: "Columns", Kind: "columns"},
		{Label: "Indexes", Kind: "indexes"},
	},
	"role": {
		{Label: "Member of", Kind: "memberof"},
		{Label: "Members", Kind: "members"},
		{Label: "Grants in this database", Kind: "grants"},
	},
}

const (
	// userSchema hides pg_catalog, pg_toast and the like, as pgAdmin does by default.
	userSchema = `n.nspname !~ '^pg_' and n.nspname <> 'information_schema'`
	// ownRole hides the roles Postgres comes with. r is pg_roles, which unlike
	// pg_authid never gives out password hashes.
	ownRole = `r.rolname !~ '^pg_'`
	// ownFunction hides functions that belong to an extension, of which there
	// can be hundreds. p is pg_proc.
	ownFunction = `p.prokind in ('f', 'p') and not exists (
		select 1 from pg_depend d
		where d.classid = 'pg_proc'::regclass and d.objid = p.oid and d.deptype = 'e')`
	// triggerFunction is true of the functions pgAdmin groups separately, as
	// they're only ever called by triggers. p is pg_proc.
	triggerFunction = `p.prorettype in ('trigger'::regtype, 'event_trigger'::regtype)`
	// ownType hides what Postgres makes by itself alongside other objects (the
	// array, multirange and row types of tables), domains, which are listed
	// apart, and types from extensions. t is pg_type.
	ownType = `t.typtype not in ('d', 'm', 'p') and t.typcategory <> 'A'
		and (t.typrelid = 0 or exists (select 1 from pg_class r where r.oid = t.typrelid and r.relkind = 'c'))
		and not ` + extensionType
	// ownDomain is the domains not from an extension. t is pg_type.
	ownDomain     = `t.typtype = 'd' and not ` + extensionType
	extensionType = `exists (select 1 from pg_depend d
		where d.classid = 'pg_type'::regclass and d.objid = t.oid and d.deptype = 'e')`
	// ownCollation hides collations from extensions. l is pg_collation.
	ownCollation = `not exists (select 1 from pg_depend d
		where d.classid = 'pg_collation'::regclass and d.objid = l.oid and d.deptype = 'e')`
	typeKind          = `case t.typtype when 'e' then 'enum' when 'c' then 'composite' when 'r' then 'range' else 'base' end`
	collationProvider = `case l.collprovider when 'i' then 'icu' when 'c' then 'libc' else 'default' end`
	// selectFrom is a query for the rows of relation c in schema n. Postgres
	// does the identifier quoting, via format's %I.
	selectFrom = `format('select * from %I.%I limit 100;', n.nspname, c.relname)`
	// tableKind is how table c expands.
	tableKind = `case c.relkind when 'p' then 'partitioned' else 'table' end`
	// rowEstimate is the planner's row count for c, which is free to read where
	// count(*) can take minutes. It's negative until the table is first analyzed.
	rowEstimate = `case when c.relkind in ('r', 'm') and c.reltuples >= 0
		then '~' || to_char(c.reltuples, 'FM999,999,999,990') else '' end`
)

// relations lists the relations of a schema matching filter. kind is the SQL
// for how each expands, and def is its definition.
func relations(filter, kind, def string) string {
	return `
		select c.relname, ` + rowEstimate + `, ` + selectFrom + `, ` + kind + `, c.oid::text, '` + def + `'
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = $1 and ` + filter + `
		order by 1`
}

// functions lists a schema's functions matching filter, with detail as the SQL
// for what to show beside each.
func functions(filter, detail string) string {
	return `
		select p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
			` + detail + `, '', '', p.oid::text, 'function'
		from pg_proc p
		join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = $1 and ` + ownFunction + ` and ` + filter + `
		order by 1`
}

// queries return one row per node: label, detail, sql, kind, id, def. A kind's
// id, if it has one, is bound as $1: a schema name, a relation's or role's oid,
// or a search term.
var queries = map[string]string{
	// Roles belong to the server, so they sit beside the schemas, not in one.
	RootKind: `
		select label, detail, '', kind, id, ''
		from (
			select n.nspname::text label, '' detail, 'schema' kind, n.nspname::text id, 1 ord
			from pg_namespace n
			where ` + userSchema + `
			union all
			select 'Roles', count(*)::text, 'roles', '', 2
			from pg_roles r
			where ` + ownRole + `
		) found
		order by ord, label collate "C"`,
	"roles": `
		select r.rolname, concat_ws(', ', case when r.rolsuper then 'superuser' end, case when r.rolcanlogin then 'login' end),
			'', 'role', r.oid::text, 'role'
		from pg_roles r
		where ` + ownRole + `
		order by 1`,
	// grouped as Postgres 16 can record a membership once per grantor.
	"memberof": `
		select r.rolname, case when bool_or(m.admin_option) then 'with admin option' else '' end,
			'', '', r.oid::text, 'role'
		from pg_auth_members m
		join pg_roles r on r.oid = m.roleid
		where m.member = $1
		group by r.oid, r.rolname
		order by 1`,
	"members": `
		select r.rolname, case when bool_or(m.admin_option) then 'with admin option' else '' end,
			'', '', r.oid::text, 'role'
		from pg_auth_members m
		join pg_roles r on r.oid = m.member
		where m.roleid = $1
		group by r.oid, r.rolname
		order by 1`,
	// An owner's rights go without saying, so those aren't listed as grants.
	// shortcut: leaves out column grants and default privileges, add them if
	// they're ever what's being looked for.
	"grants": `
		select o.what, string_agg(lower(a.privilege_type), ', ' order by a.privilege_type), '', '', '', ''
		from (
			select 1 ord, 'database ' || quote_ident(d.datname) what, d.datacl acl, d.datdba owner
			from pg_database d
			where d.datname = current_database()
			union all
			select 2, 'schema ' || quote_ident(n.nspname), n.nspacl, n.nspowner
			from pg_namespace n
			where ` + userSchema + `
			union all
			select 3, case c.relkind when 'S' then 'sequence ' when 'v' then 'view ' when 'm' then 'matview ' else 'table ' end
					|| format('%I.%I', n.nspname, c.relname),
				c.relacl, c.relowner
			from pg_class c
			join pg_namespace n on n.oid = c.relnamespace
			where c.relkind in ('r', 'p', 'v', 'm', 'S', 'f') and c.relacl is not null and ` + userSchema + `
			union all
			select 4, format('function %I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)),
				p.proacl, p.proowner
			from pg_proc p
			join pg_namespace n on n.oid = p.pronamespace
			where p.proacl is not null and ` + ownFunction + ` and ` + userSchema + `
		) o
		cross join lateral aclexplode(o.acl) a
		where a.grantee = $1 and a.grantee <> o.owner
		group by o.ord, o.what
		order by o.ord, o.what`,
	"schema": `
		select g.label, g.total::text, '', g.kind, n.nspname::text, ''
		from pg_namespace n
		cross join lateral (values
			(1, 'Tables', 'tables', (select count(*) from pg_class c where c.relnamespace = n.oid and c.relkind in ('r', 'p') and not c.relispartition)),
			(2, 'Views', 'views', (select count(*) from pg_class c where c.relnamespace = n.oid and c.relkind = 'v')),
			(3, 'Materialized views', 'matviews', (select count(*) from pg_class c where c.relnamespace = n.oid and c.relkind = 'm')),
			(4, 'Functions', 'functions', (select count(*) from pg_proc p where p.pronamespace = n.oid and ` + ownFunction + ` and not ` + triggerFunction + `)),
			(5, 'Trigger functions', 'triggerfunctions', (select count(*) from pg_proc p where p.pronamespace = n.oid and ` + ownFunction + ` and ` + triggerFunction + `)),
			(6, 'Sequences', 'sequences', (select count(*) from pg_class c where c.relnamespace = n.oid and c.relkind = 'S')),
			(7, 'Collations', 'collations', (select count(*) from pg_collation l where l.collnamespace = n.oid and ` + ownCollation + `)),
			(8, 'Domains', 'domains', (select count(*) from pg_type t where t.typnamespace = n.oid and ` + ownDomain + `)),
			(9, 'Types', 'types', (select count(*) from pg_type t where t.typnamespace = n.oid and ` + ownType + `))
		) g(ord, label, kind, total)
		where n.nspname = $1
		order by g.ord`,
	// Partitions are listed under their parent, as pgAdmin does.
	"tables":    relations(`c.relkind in ('r', 'p') and not c.relispartition`, tableKind, "table"),
	"views":     relations(`c.relkind = 'v'`, `'view'`, "view"),
	"matviews":  relations(`c.relkind = 'm'`, `'matview'`, "matview"),
	"sequences": relations(`c.relkind = 'S'`, `''`, "sequence"),
	"partitions": `
		select c.relname, ` + rowEstimate + `, ` + selectFrom + `, ` + tableKind + `, c.oid::text, 'table'
		from pg_inherits i
		join pg_class c on c.oid = i.inhrelid
		join pg_namespace n on n.oid = c.relnamespace
		where i.inhparent = $1
		order by 1`,
	"functions":        functions(`not `+triggerFunction, `pg_get_function_result(p.oid)`),
	"triggerfunctions": functions(triggerFunction, `''`),
	"collations": `
		select l.collname, ` + collationProvider + `, '', '', l.oid::text, 'collation'
		from pg_collation l
		join pg_namespace n on n.oid = l.collnamespace
		where n.nspname = $1 and ` + ownCollation + `
		order by 1`,
	"domains": `
		select t.typname, format_type(t.typbasetype, t.typtypmod), '', '', t.oid::text, 'domain'
		from pg_type t
		join pg_namespace n on n.oid = t.typnamespace
		where n.nspname = $1 and ` + ownDomain + `
		order by 1`,
	"types": `
		select t.typname, ` + typeKind + `, '', '', t.oid::text, 'type'
		from pg_type t
		join pg_namespace n on n.oid = t.typnamespace
		where n.nspname = $1 and ` + ownType + `
		order by 1`,
	"columns": `
		select attname,
			format_type(atttypid, atttypmod) || case when attnotnull then ' not null' else '' end,
			'', '', '', ''
		from pg_attribute
		where attrelid = $1 and attnum > 0 and not attisdropped
		order by attnum`,
	"indexes": `
		select c.relname, pg_get_indexdef(i.indexrelid), '', '', '', ''
		from pg_index i
		join pg_class c on c.oid = i.indexrelid
		where i.indrelid = $1
		order by 1`,
	"constraints": `
		select conname, pg_get_constraintdef(oid), '', '', '', ''
		from pg_constraint
		where conrelid = $1
		order by 1`,
	"triggers": `
		select tgname, pg_get_triggerdef(oid), '', '', '', ''
		from pg_trigger
		where tgrelid = $1 and not tgisinternal
		order by 1`,
	// position rather than like, so % and _ in the term are literal.
	// shortcut: stops at 200 matches, add paging if that proves too few.
	SearchKind: `
		select * from (
			select c.relname, n.nspname::text, ` + selectFrom + `,
				case c.relkind when 'v' then 'view' when 'm' then 'matview' when 'S' then '' when 'p' then 'partitioned' else 'table' end,
				c.oid::text,
				case c.relkind when 'v' then 'view' when 'm' then 'matview' when 'S' then 'sequence' else 'table' end
			from pg_class c
			join pg_namespace n on n.oid = c.relnamespace
			where c.relkind in ('r', 'p', 'v', 'm', 'S') and ` + userSchema + `
				and position(lower($1) in lower(c.relname)) > 0
			union all
			select p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
				n.nspname::text, '', '', p.oid::text, 'function'
			from pg_proc p
			join pg_namespace n on n.oid = p.pronamespace
			where ` + ownFunction + ` and ` + userSchema + `
				and position(lower($1) in lower(p.proname)) > 0
			union all
			select l.collname, n.nspname::text, '', '', l.oid::text, 'collation'
			from pg_collation l
			join pg_namespace n on n.oid = l.collnamespace
			where ` + ownCollation + ` and ` + userSchema + `
				and position(lower($1) in lower(l.collname)) > 0
			union all
			select t.typname, n.nspname::text, '', '', t.oid::text,
				case t.typtype when 'd' then 'domain' else 'type' end
			from pg_type t
			join pg_namespace n on n.oid = t.typnamespace
			where (` + ownDomain + ` or ` + ownType + `) and ` + userSchema + `
				and position(lower($1) in lower(t.typname)) > 0
			union all
			select r.rolname, 'role', '', '', r.oid::text, 'role'
			from pg_roles r
			where ` + ownRole + `
				and position(lower($1) in lower(r.rolname)) > 0
		) found
		order by 1, 2
		limit 200`,
}

// definitions return one row and column: the object's SQL. $1 is its oid.
var definitions = map[string]string{
	// shortcut: a list setting such as search_path comes out as one quoted
	// value, which is fine to read but needs splitting before it's run.
	"role": `
		select concat_ws(E'\n',
			'-- Rebuilt from the catalogs: passwords are never shown.',
			format('create role %I%s;', r.rolname, concat(
				case when r.rolsuper then ' superuser' end,
				case when r.rolcanlogin then ' login' end,
				case when r.rolcreatedb then ' createdb' end,
				case when r.rolcreaterole then ' createrole' end,
				case when r.rolreplication then ' replication' end,
				case when r.rolbypassrls then ' bypassrls' end,
				case when not r.rolinherit then ' noinherit' end,
				case when r.rolconnlimit >= 0 then ' connection limit ' || r.rolconnlimit end,
				case when r.rolvaliduntil is not null then format(' valid until %L', r.rolvaliduntil) end)),
			(select string_agg(format('alter role %I set %s to %L;', r.rolname, split_part(s, '=', 1), substr(s, position('=' in s) + 1)), E'\n')
				from unnest(r.rolconfig) s),
			(select string_agg(format('grant %I to %I%s;', g.rolname, r.rolname, case when m.admin then ' with admin option' else '' end), E'\n' order by g.rolname)
				from (
					select roleid, bool_or(admin_option) admin
					from pg_auth_members
					where member = r.oid
					group by roleid
				) m
				join pg_roles g on g.oid = m.roleid))
		from pg_roles r
		where r.oid = $1`,
	"function": `select rtrim(pg_get_functiondef($1::oid), E'\n') || ';'`,
	"view": `
		select format(E'create or replace view %I.%I as\n%s', n.nspname, c.relname, pg_get_viewdef(c.oid, true))
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where c.oid = $1`,
	"matview": `
		select format(E'create materialized view %I.%I as\n%s', n.nspname, c.relname, pg_get_viewdef(c.oid, true))
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where c.oid = $1`,
	"sequence": `
		select format('create sequence %I.%I as %s increment %s minvalue %s maxvalue %s start %s cache %s%s;',
			n.nspname, c.relname, format_type(s.seqtypid, null), s.seqincrement, s.seqmin, s.seqmax,
			s.seqstart, s.seqcache, case when s.seqcycle then ' cycle' else '' end)
		from pg_sequence s
		join pg_class c on c.oid = s.seqrelid
		join pg_namespace n on n.oid = c.relnamespace
		where s.seqrelid = $1`,
	// The ICU locale's column has been renamed between versions, hence to_jsonb.
	"collation": `
		select format('create collation %I.%I (provider = %s, locale = %L%s);', n.nspname, l.collname,
			` + collationProvider + `,
			coalesce(to_jsonb(l) ->> 'colllocale', to_jsonb(l) ->> 'colliculocale', l.collcollate),
			case when l.collisdeterministic then '' else ', deterministic = false' end)
		from pg_collation l
		join pg_namespace n on n.oid = l.collnamespace
		where l.oid = $1`,
	"domain": `
		select format('create domain %I.%I as %s%s%s%s;', n.nspname, t.typname,
			format_type(t.typbasetype, t.typtypmod),
			case when t.typdefault is not null then ' default ' || t.typdefault else '' end,
			case when t.typnotnull then ' not null' else '' end,
			coalesce((
				select string_agg(format(E'\n    constraint %I %s', k.conname, pg_get_constraintdef(k.oid)), '' order by k.conname)
				from pg_constraint k
				where k.contypid = t.oid and k.contype <> 'n'), ''))
		from pg_type t
		join pg_namespace n on n.oid = t.typnamespace
		where t.oid = $1`,
	"type": `
		select case t.typtype
			when 'e' then format(E'create type %I.%I as enum (\n%s\n);', n.nspname, t.typname, (
				select string_agg('    ' || quote_literal(e.enumlabel), E',\n' order by e.enumsortorder)
				from pg_enum e
				where e.enumtypid = t.oid))
			when 'c' then format(E'create type %I.%I as (\n%s\n);', n.nspname, t.typname, (
				select string_agg(format('    %I %s', a.attname, format_type(a.atttypid, a.atttypmod)), E',\n' order by a.attnum)
				from pg_attribute a
				where a.attrelid = t.typrelid and a.attnum > 0 and not a.attisdropped))
			when 'r' then format('create type %I.%I as range (subtype = %s%s);', n.nspname, t.typname,
				format_type(g.rngsubtype, null),
				case when g.rngsubdiff <> 0 then ', subtype_diff = ' || g.rngsubdiff::regproc else '' end)
			else format('create type %I.%I (input = %s, output = %s, internallength = %s);', n.nspname, t.typname,
				t.typinput, t.typoutput, case when t.typlen < 0 then 'variable' else t.typlen::text end)
			end
		from pg_type t
		join pg_namespace n on n.oid = t.typnamespace
		left join pg_range g on g.rngtypid = t.oid
		where t.oid = $1`,
	// Postgres has no pg_get_tabledef, so this one is put together by hand.
	// shortcut: leaves out inheritance, storage options and grants, which its
	// first line says. Use pg_dump's output if those ever matter here.
	"table": `
		select format(E'-- Rebuilt from the catalogs: no inheritance, storage options or grants.\ncreate table %I.%I (\n%s\n)%s;%s%s',
			n.nspname, c.relname,
			array_to_string(cols.lines || cons.lines, E',\n'),
			case when c.relkind = 'p' then ' partition by ' || pg_get_partkeydef(c.oid) else '' end,
			coalesce(idx.lines, ''),
			coalesce(trg.lines, ''))
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		cross join lateral (
			select coalesce(array_agg(format('    %I %s%s%s%s', a.attname, format_type(a.atttypid, a.atttypmod),
				case a.attidentity when 'a' then ' generated always as identity' when 'd' then ' generated by default as identity' else '' end,
				case
					when a.attgenerated <> '' then format(' generated always as (%s)%s', pg_get_expr(d.adbin, d.adrelid),
						case a.attgenerated when 's' then ' stored' else ' virtual' end)
					when d.adbin is not null then ' default ' || pg_get_expr(d.adbin, d.adrelid)
					else ''
				end,
				case when a.attnotnull then ' not null' else '' end) order by a.attnum), '{}') lines
			from pg_attribute a
			left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
			where a.attrelid = c.oid and a.attnum > 0 and not a.attisdropped
		) cols
		cross join lateral (
			select coalesce(array_agg(format('    constraint %I %s', k.conname, pg_get_constraintdef(k.oid)) order by k.conname), '{}') lines
			from pg_constraint k
			where k.conrelid = c.oid and k.contype <> 'n'
		) cons
		cross join lateral (
			select string_agg(E'\n' || pg_get_indexdef(i.indexrelid) || ';', '' order by i.indexrelid) lines
			from pg_index i
			where i.indrelid = c.oid
				and not exists (select 1 from pg_constraint k where k.conrelid = c.oid and k.conindid = i.indexrelid)
		) idx
		cross join lateral (
			select string_agg(E'\n' || pg_get_triggerdef(t.oid) || ';', '' order by t.tgname) lines
			from pg_trigger t
			where t.tgrelid = c.oid and not t.tgisinternal
		) trg
		where c.oid = $1`,
}

// Catalog reads the tree over one connection, opened on first use and
// reopened if lost.
type Catalog struct {
	connect func(context.Context) (*pgconn.PgConn, error)

	mu     sync.Mutex
	conn   *pgconn.PgConn // guarded by mu
	closed bool           // guarded by mu
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
	if kind == SearchKind && id == "" {
		kind = RootKind
	}
	sql, ok := queries[kind]
	if !ok {
		return nil, fmt.Errorf("unknown tree node kind %q", kind)
	}
	var params [][]byte
	if id != "" {
		params = [][]byte{[]byte(id)}
	}
	rows, err := c.read(ctx, sql, params)
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, len(rows))
	for i, r := range rows {
		nodes[i] = Node{
			Label:  string(r[0]),
			Detail: string(r[1]),
			SQL:    string(r[2]),
			Kind:   string(r[3]),
			ID:     string(r[4]),
			Def:    string(r[5]),
		}
	}
	return nodes, nil
}

// Definition returns the SQL that defines a node, given its Def and ID.
func (c *Catalog) Definition(ctx context.Context, def, id string) (string, error) {
	sql, ok := definitions[def]
	if !ok {
		return "", fmt.Errorf("unknown definition %q", def)
	}
	rows, err := c.read(ctx, sql, [][]byte{[]byte(id)})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("object no longer exists")
	}
	return string(rows[0][0]), nil
}

// Completions is what the editor's autocomplete works from.
type Completions struct {
	DefaultSchema string                         `json:"defaultSchema"` // where unqualified names resolve; empty if nowhere
	Schemas       map[string]map[string][]string `json:"schemas"`       // columns by schema and table
}

// Completions returns the columns of every table and view.
func (c *Catalog) Completions(ctx context.Context) (Completions, error) {
	rows, err := c.read(ctx, `
		select n.nspname, c.relname, a.attname, current_schema()
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		join pg_attribute a on a.attrelid = c.oid
		where c.relkind in ('r', 'p', 'v', 'm') and a.attnum > 0 and not a.attisdropped and `+userSchema+`
		order by 1, 2, a.attnum`, nil)
	if err != nil {
		return Completions{}, err
	}
	done := Completions{Schemas: map[string]map[string][]string{}}
	for _, r := range rows {
		schema, table := string(r[0]), string(r[1])
		if done.Schemas[schema] == nil {
			done.Schemas[schema] = map[string][]string{}
		}
		done.Schemas[schema][table] = append(done.Schemas[schema][table], string(r[2]))
		done.DefaultSchema = string(r[3])
	}
	return done, nil
}

// read runs sql on the catalog's connection, opening it if need be.
func (c *Catalog) read(ctx context.Context, sql string, params [][]byte) ([][][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.conn == nil || !conn.Alive(ctx, c.conn) {
		fresh, err := c.connect(ctx)
		if err != nil {
			return nil, err
		}
		c.conn = fresh
	}
	res := c.conn.ExecParams(ctx, sql, params, nil, nil, nil).Read()
	return res.Rows, res.Err
}

// Close closes the connection, if one was opened.
func (c *Catalog) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	return c.conn.Close(ctx)
}
