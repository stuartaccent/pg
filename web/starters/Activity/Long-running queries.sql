-- Anything busy for over a minute, and what it's waiting on, if anything.
select pid, usename as user, datname as database, state,
	date_trunc('second', clock_timestamp() - query_start) as running_for,
	wait_event_type, wait_event, query
from pg_stat_activity
where state <> 'idle' and pid <> pg_backend_pid() and query_start < clock_timestamp() - interval '1 minute'
order by query_start;
