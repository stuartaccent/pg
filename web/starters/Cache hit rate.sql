-- Share of reads served from memory. Well under 99% on a busy database can
-- mean it needs more of it.
select datname as database, blks_hit, blks_read,
	round(100.0 * blks_hit / nullif(blks_hit + blks_read, 0), 2) as hit_percent
from pg_stat_database
where datname = current_database();
