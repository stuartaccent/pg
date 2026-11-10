-- Left behind by a create index concurrently that failed. Queries never use
-- them, but every write still updates them: drop and build them again.
select n.nspname as schema, t.relname as table, c.relname as index,
	pg_size_pretty(pg_relation_size(c.oid)) as size
from pg_index i
join pg_class c on c.oid = i.indexrelid
join pg_class t on t.oid = i.indrelid
join pg_namespace n on n.oid = t.relnamespace
where not i.indisvalid
	and c.relkind <> 'I' -- a partitioned index made on only the parent, waiting for its partitions
order by 1, 2, 3;
