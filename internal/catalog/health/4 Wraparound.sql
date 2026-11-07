-- Databases older than autovacuum_freeze_max_age, which autovacuum should
-- have frozen by now. Postgres stops all writes at 100% of the limit.
select datname as database, age(datfrozenxid) as transactions_old,
	round(100.0 * age(datfrozenxid) / 2147483647, 1) as percent_of_limit
from pg_database
where age(datfrozenxid) > current_setting('autovacuum_freeze_max_age')::bigint
order by age(datfrozenxid) desc;
