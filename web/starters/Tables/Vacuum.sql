-- Tables longest without a vacuum, with their dead rows and last analyze.
-- Many dead rows and an old vacuum mean autovacuum isn't keeping up.
select schemaname as schema, relname as table, n_live_tup as live_rows, n_dead_tup as dead_rows,
	date_trunc('minute', greatest(last_vacuum, last_autovacuum)) as last_vacuum,
	date_trunc('minute', greatest(last_analyze, last_autoanalyze)) as last_analyze,
	autovacuum_count + vacuum_count as vacuums
from pg_stat_user_tables
order by greatest(last_autovacuum, last_vacuum) nulls first, n_dead_tup desc
limit 25;
