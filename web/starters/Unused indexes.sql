-- Scans are counted since the statistics were last reset, and on this server
-- only. Unique indexes are left out, as they do a job without being scanned.
select s.schemaname as schema, s.relname as table, s.indexrelname as index,
	pg_size_pretty(pg_relation_size(s.indexrelid)) as size, s.idx_scan as scans
from pg_stat_user_indexes s
join pg_index i on i.indexrelid = s.indexrelid
where s.idx_scan = 0 and not i.indisunique
order by pg_relation_size(s.indexrelid) desc;
