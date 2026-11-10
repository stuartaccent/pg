-- No index helps find the rows that use a referenced row, so deleting or
-- updating one scans this whole table to check it, and so do joins on it. An
-- index helps if it starts with the foreign key's first column, or is unique
-- on some of its columns, even if it only covers rows where they're not null.
-- A small referenced table that's rarely deleted from is usually fine.
select n.nspname as schema, t.relname as table, c.conname as foreign_key,
	(select string_agg(a.attname, ', ' order by k.ord)
	 from unnest(c.conkey) with ordinality k(attnum, ord)
	 join pg_attribute a on a.attrelid = c.conrelid and a.attnum = k.attnum) as columns,
	c.confrelid::regclass::text as "references",
	nullif(greatest(p.reltuples, -1), -1)::bigint as references_rows,
	pg_size_pretty(pg_relation_size(t.oid)) as table_size
from pg_constraint c
join pg_class t on t.oid = c.conrelid
join pg_class p on p.oid = c.confrelid
join pg_namespace n on n.oid = t.relnamespace
where c.contype = 'f'
	and c.conparentid = 0 -- not a partition's copy of its parent's key
	and n.nspname not in ('pg_catalog', 'information_schema')
	and not exists (
		select from pg_index i
		cross join lateral (select string_to_array(i.indkey::text, ' ')::int2[] as cols) k
		where i.indrelid = c.conrelid and i.indisvalid
			and (k.cols[1] = c.conkey[1]
				or k.cols[1:cardinality(c.conkey)] @> c.conkey
				or (i.indisunique and c.conkey @> k.cols[1:i.indnkeyatts]))
			-- The check only looks up values that aren't null, so a partial
			-- index leaving out nulls in the key's columns serves it too.
			and (i.indpred is null or not exists (
				select from regexp_split_to_table(pg_get_expr(i.indpred, i.indrelid), ' AND ') part
				where btrim(part, '() ') not in (
					select quote_ident(a.attname) || ' IS NOT NULL'
					from pg_attribute a
					where a.attrelid = c.conrelid and a.attnum = any(c.conkey))
			))
	)
order by pg_relation_size(t.oid) desc, 1, 2, 3;
