-- Vacuums running now, autovacuum's included, and how far through they are.
select p.pid, p.datname as database, p.relid::regclass as table, p.phase,
	round(100.0 * p.heap_blks_scanned / nullif(p.heap_blks_total, 0), 1) as scanned_pct,
	date_trunc('second', now() - a.query_start) as running_for
from pg_stat_progress_vacuum p
join pg_stat_activity a using (pid);
