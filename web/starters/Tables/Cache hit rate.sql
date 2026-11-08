-- Tables doing the most reads that miss memory, with the share that hit it,
-- for the table and its indexes. Sorted by misses, as a low rate on a table
-- that's barely read doesn't matter. Counted since the stats were last reset.
select schemaname as schema, relname as table,
	heap_blks_hit + heap_blks_read as table_reads,
	heap_blks_read as table_misses,
	round(100.0 * heap_blks_hit / nullif(heap_blks_hit + heap_blks_read, 0), 2) as table_hit_pct,
	coalesce(idx_blks_hit, 0) + coalesce(idx_blks_read, 0) as index_reads,
	idx_blks_read as index_misses,
	round(100.0 * idx_blks_hit / nullif(idx_blks_hit + idx_blks_read, 0), 2) as index_hit_pct
from pg_statio_user_tables
order by coalesce(heap_blks_read, 0) + coalesce(idx_blks_read, 0) desc
limit 20;
