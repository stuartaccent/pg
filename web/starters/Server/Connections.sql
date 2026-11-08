-- Who's connected, by database, user and state. Many "idle in transaction",
-- or long ones, hold locks and stop vacuum cleaning up.
select datname as database, usename as user, state, count(*) as connections,
	max(date_trunc('second', now() - state_change)) as longest_in_state
from pg_stat_activity
where backend_type = 'client backend'
group by 1, 2, 3
order by connections desc;
