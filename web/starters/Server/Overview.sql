-- The server at a glance: version, how long it's been up, connections
-- against the limit, and the size of every database together.
select version() as version,
	date_trunc('second', now() - pg_postmaster_start_time()) as uptime,
	(select count(*) from pg_stat_activity where backend_type = 'client backend') as connections,
	current_setting('max_connections')::int as max_connections,
	pg_size_pretty(sum(pg_database_size(datname))) as all_databases
from pg_database;
