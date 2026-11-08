-- Queries waiting on a lock, and who holds it. Run this when something seems stuck.
select waiting.pid as waiting_pid, waiting.usename as waiting_user,
	date_trunc('second', now() - waiting.query_start) as waiting_for,
	left(waiting.query, 80) as waiting_query,
	blocker.pid as blocking_pid, blocker.usename as blocking_user, blocker.state as blocking_state,
	left(blocker.query, 80) as blocking_query
from pg_stat_activity waiting
cross join lateral unnest(pg_blocking_pids(waiting.pid)) as b(pid)
join pg_stat_activity blocker on blocker.pid = b.pid
order by waiting.query_start;
