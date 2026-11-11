#!/bin/sh
# Prints the SQL to run in backup B's database so its public schema matches backup A's,
# e.g. what a staging copy needs to catch up with local, without running the migrations.
# Both are pg's .backup files (pg_dump custom format; schema-only is enough). They're
# restored into a throwaway Postgres container, diffed with pgschema, and it's removed.
#
#   scripts/schemadiff.sh local.backup staging.backup > staging-catch-up.sql
#
# Review the SQL before running it. pgschema plans DDL only. It replaces functions after
# validating new check constraints, so move a CREATE OR REPLACE FUNCTION above a check
# that needs its new body. And a changed function doesn't re-check rows already in place.
#
# Not compared, so check these by hand: collations (A's are only copied in for pgschema to
# plan with, so one B lacks, like spectra's "numeric", won't show up and its SQL will fail
# in B) and every schema but public (audit, mcp).
set -eu

[ $# -eq 2 ] || { echo "usage: $0 a.backup b.backup  (prints what b needs to match a)" >&2; exit 2; }
a=$1 b=$2
[ -f "$a" ] && [ -f "$b" ] || { echo "no such backup: $a or $b" >&2; exit 2; }

# pg_restore can't read a dump newer than itself, so take the newer of the versions pg
# puts in backup names (-pg18-20261111-204500.backup), or the latest for other files.
version=$(printf '%s\n%s\n' "$a" "$b" | sed -n 's/.*-pg\([0-9][0-9]*\)-[0-9]\{8\}-[0-9]\{6\}\.backup$/\1/p' | sort -n | tail -1)
image=postgres:${version:-latest}

tmp=$(mktemp -d)
container=$(docker run -d --rm -e POSTGRES_HOST_AUTH_METHOD=trust -p 127.0.0.1::5432 "$image")
trap 'docker rm -f "$container" >/dev/null; rm -rf "$tmp"' EXIT

# The image's first start is a socket-only server for initdb; TCP answers once the real one is up.
until docker exec "$container" pg_isready -q -U postgres -h 127.0.0.1; do sleep 1; done

pg() { docker exec -i -e PGOPTIONS='-c client_min_messages=warning' "$container" "$@"; }
for db in a b plan; do pg createdb -U postgres "$db"; done
pg pg_restore -U postgres --schema-only --no-owner --no-privileges --exit-on-error -d a < "$a"
pg pg_restore -U postgres --schema-only --no-owner --no-privileges --exit-on-error -d b < "$b"

# pgschema plans by loading A's public into "plan", so that needs what public leans on:
# the other schemas (spectra's triggers call audit's functions), the extensions and
# public's collations, none of which its dump of public carries.
# Objects there that lean on public in turn (spectra's mcp views) can't load without it and
# aren't needed, so their errors are dropped; anything public did need, pgschema names when it fails.
pg sh -c 'pg_dump -U postgres --schema-only -N public a | psql -U postgres -q -o /dev/null -d plan 2>/dev/null'
pg psql -U postgres -At -v ON_ERROR_STOP=1 -d a <<'SQL' | pg psql -U postgres -q -v ON_ERROR_STOP=1 -d plan
select format('create extension if not exists %I with schema %I;', extname, extnamespace::regnamespace)
from pg_extension
where extname <> 'plpgsql'
union all
select format('create collation if not exists %I.%I (provider = %s, locale = %L, deterministic = %s);',
              collnamespace::regnamespace, collname,
              case collprovider when 'i' then 'icu' when 'b' then 'builtin' else 'libc' end,
              -- the ICU locale's column was renamed in 17; to_jsonb reads whichever this server has
              coalesce(to_jsonb(c) ->> 'colllocale', to_jsonb(c) ->> 'colliculocale', collcollate), collisdeterministic::text)
from pg_collation c
where collnamespace = 'public'::regnamespace;
SQL

port=$(docker port "$container" 5432/tcp | head -1 | sed 's/.*://')
export PGHOST=127.0.0.1 PGPORT="$port" PGUSER=postgres
export PGSCHEMA_PLAN_HOST=127.0.0.1 PGSCHEMA_PLAN_PORT="$port" PGSCHEMA_PLAN_USER=postgres PGSCHEMA_PLAN_DB=plan
pgschema() { go run github.com/pgplex/pgschema@v1.14.1 "$@"; }
pgschema dump --db a > "$tmp/a.sql"
pgschema plan --db b --file "$tmp/a.sql" --output-sql stdout
