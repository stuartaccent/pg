-- Each standby and how far behind it is. Empty without any.
select client_addr, application_name, state, sync_state,
	pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)) as replay_behind,
	replay_lag
from pg_stat_replication;
