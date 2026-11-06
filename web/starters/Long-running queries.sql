select pid, usename as user, datname as database, state,
	clock_timestamp() - query_start as running_for, query
from pg_stat_activity
where state <> 'idle' and pid <> pg_backend_pid() and query_start < clock_timestamp() - interval '1 minute'
order by query_start;
