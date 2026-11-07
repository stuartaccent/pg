-- No index starts with the foreign key's columns, so deleting or updating a
-- referenced row scans this whole table to check it, and so do joins on it.
-- A unique index on some of them does as well, as it finds at most one row.
select n.nspname as schema, t.relname as table, c.conname as foreign_key,
	(select string_agg(a.attname, ', ' order by k.ord)
	 from unnest(c.conkey) with ordinality k(attnum, ord)
	 join pg_attribute a on a.attrelid = c.conrelid and a.attnum = k.attnum) as columns,
	c.confrelid::regclass::text as "references",
	pg_size_pretty(pg_relation_size(t.oid)) as table_size
from pg_constraint c
join pg_class t on t.oid = c.conrelid
join pg_namespace n on n.oid = t.relnamespace
where c.contype = 'f'
	and n.nspname not in ('pg_catalog', 'information_schema')
	and not exists (
		select from pg_index i
		where i.indrelid = c.conrelid and i.indisvalid and i.indpred is null
			and ((string_to_array(i.indkey::text, ' ')::int2[])[1:cardinality(c.conkey)] @> c.conkey
				or (i.indisunique and c.conkey @> (string_to_array(i.indkey::text, ' ')::int2[])[1:i.indnkeyatts]))
	)
order by pg_relation_size(t.oid) desc, 1, 2, 3;
