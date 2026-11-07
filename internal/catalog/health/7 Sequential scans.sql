-- Usage: tables of over 10,000 rows read more often by sequential scan than
-- by index, most rows read first. Often a sign of a missing index.
select schemaname as schema, relname as table, n_live_tup as rows,
	seq_scan as sequential_scans, coalesce(idx_scan, 0) as index_scans,
	seq_tup_read as rows_read_sequentially
from pg_stat_user_tables
where n_live_tup > 10000 and seq_scan > 10 and seq_scan > coalesce(idx_scan, 0)
order by seq_tup_read desc
limit 20;
