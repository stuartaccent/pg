-- The queries taking the most time in total, which often matter more than the
-- slowest single one. Needs pg_stat_statements loaded through
-- shared_preload_libraries (RDS does), then create extension pg_stat_statements.
select round(total_exec_time::numeric / 1000, 1) as total_seconds, calls,
	round((total_exec_time / calls)::numeric, 2) as avg_ms, rows,
	round(100.0 * shared_blks_hit / nullif(shared_blks_hit + shared_blks_read, 0), 1) as cache_hit_pct,
	left(regexp_replace(query, '\s+', ' ', 'g'), 120) as query
from pg_stat_statements
order by total_exec_time desc
limit 20;
