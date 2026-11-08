-- Settings worth knowing. setting is in unit: shared_buffers of 16384 with a
-- unit of 8kB is 128MB. source says where it was set.
select name, setting, unit, source
from pg_settings
where name in ('shared_buffers', 'effective_cache_size', 'work_mem', 'maintenance_work_mem',
	'max_connections', 'max_wal_size', 'checkpoint_timeout', 'random_page_cost',
	'autovacuum_max_workers', 'autovacuum_naptime', 'statement_timeout',
	'idle_in_transaction_session_timeout')
order by name;
