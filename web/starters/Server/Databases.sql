-- Every database: size, share of reads served from memory (well under 99% on
-- a busy one can mean it needs more), rollbacks, deadlocks, and temp files,
-- which are sorts and hashes too big for work_mem. Counted since stats_since.
select datname as database,
	pg_size_pretty(pg_database_size(datname)) as size,
	round(100.0 * blks_hit / nullif(blks_hit + blks_read, 0), 2) as cache_hit_pct,
	xact_commit as commits, xact_rollback as rollbacks,
	round(100.0 * xact_rollback / nullif(xact_commit + xact_rollback, 0), 2) as rollback_pct,
	deadlocks, conflicts,
	temp_files, pg_size_pretty(temp_bytes) as temp_written,
	date_trunc('second', stats_reset) as stats_since
from pg_stat_database
where datname is not null
order by pg_database_size(datname) desc;
