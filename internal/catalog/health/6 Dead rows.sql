-- Usage: over 10% of the rows are dead, so vacuum is falling behind, and the
-- table is bigger and slower than it needs to be.
select schemaname as schema, relname as table, n_live_tup as live_rows, n_dead_tup as dead_rows,
	round(100.0 * n_dead_tup / (n_live_tup + n_dead_tup), 1) as percent_dead,
	to_char(greatest(last_vacuum, last_autovacuum), 'YYYY-MM-DD HH24:MI') as last_vacuum
from pg_stat_user_tables
where n_dead_tup > 1000 and n_dead_tup > 0.1 * (n_live_tup + n_dead_tup)
order by n_dead_tup desc;
