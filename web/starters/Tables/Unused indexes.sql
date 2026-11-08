-- Indexes never scanned, biggest first: they slow every write and take space.
-- Unique and exclusion indexes are left out, as they enforce a rule without
-- being scanned. Scans are counted since the stats were last reset, and on
-- this server only, so check any replicas before dropping one.
select s.schemaname as schema, s.relname as table, s.indexrelname as index,
	pg_size_pretty(pg_relation_size(s.indexrelid)) as size
from pg_stat_user_indexes s
join pg_index i on i.indexrelid = s.indexrelid
where s.idx_scan = 0 and not i.indisunique and not i.indisexclusion
order by pg_relation_size(s.indexrelid) desc;
