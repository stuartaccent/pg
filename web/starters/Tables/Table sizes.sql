select n.nspname as schema, c.relname as table,
	pg_size_pretty(pg_total_relation_size(c.oid)) as total,
	pg_size_pretty(pg_relation_size(c.oid)) as data,
	pg_size_pretty(pg_indexes_size(c.oid)) as indexes,
	c.reltuples::bigint as estimated_rows
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'm') and n.nspname !~ '^pg_' and n.nspname <> 'information_schema'
order by pg_total_relation_size(c.oid) desc;
