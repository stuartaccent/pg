-- Indexes on the same columns, in the same way, for the same rows. Every write
-- updates them all, so keep one and drop the rest (a unique one if there is one).
select n.nspname as schema, t.relname as table,
	string_agg(c.relname, ', ' order by i.indisunique desc, c.relname) as indexes,
	pg_size_pretty(sum(pg_relation_size(c.oid))) as total_size
from pg_index i
join pg_class c on c.oid = i.indexrelid
join pg_class t on t.oid = i.indrelid
join pg_namespace n on n.oid = t.relnamespace
where n.nspname not in ('pg_catalog', 'information_schema') and n.nspname !~ '^pg_toast'
group by n.nspname, t.relname, i.indrelid, i.indkey::text, i.indclass::text, i.indcollation::text, i.indoption::text,
	pg_get_expr(i.indexprs, i.indrelid), pg_get_expr(i.indpred, i.indrelid)
having count(*) > 1
order by sum(pg_relation_size(c.oid)) desc;
