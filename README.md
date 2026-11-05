# pg

A personal Postgres IDE: Wails v3 window, templ + HTMX UI, pgx underneath.

Connections come from `~/.pg_service.conf` (or `$PGSERVICEFILE`), passwords from `~/.pgpass`, so the app stores no credentials of its own.

```ini
# ~/.pg_service.conf
[local]
host=localhost
port=5432
dbname=postgres
user=stuart
```

To make a service read-only, for production say, add this line to it. pg shows a badge when it's on. It's a safety net rather than a lock, as `set default_transaction_read_only = off` turns it back off (Postgres 14 or later is needed for the badge).

```ini
options=-c default_transaction_read_only=on
```

## Run

```sh
go run ./cmd/pgide          # the desktop app (needs Xcode command line tools for cgo)
```

## Work on the UI

```sh
templ generate --watch      # in one terminal
go run ./cmd/dev            # in another, then open http://127.0.0.1:8080
tmp/tailwindcss -i web/css/app.css -o web/static/app.css --minify --watch   # when changing classes
```

The templ CLI version should match the `github.com/a-h/templ` version in go.mod.

The SQL editor is CodeMirror 6, bundled from `web/js/app.js` into `web/static/app.js`, which is generated and kept in the repo. Node is only needed to change it:

```sh
npm ci && npm run build && npm test
```

Styling is daisyUI 5.7.46 (vendored as `web/css/daisyui.mjs`) on the Tailwind standalone CLI. `web/static/app.css` is generated, so edit `web/css/app.css` instead. The CLI isn't kept in the repo; on Apple silicon, fetch it once with:

```sh
mkdir -p tmp && curl -fsSLo tmp/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/v4.3.3/tailwindcss-macos-arm64 && chmod +x tmp/tailwindcss
```

## Test

The session tests need a real server and are skipped otherwise:

```sh
PGIDE_TEST_SERVICE=local go test -race ./...
```

## Using it

- ⌘/Ctrl+Enter runs the selection, or the whole editor if nothing's selected. So do Explain, Analyze and Export CSV.
- Autocomplete offers the columns of the tables in the statement under the cursor, wherever it is in the statement.
- Config opens the service and password files in your text editor, creating them if need be. pg never reads them itself beyond connecting, and new services appear when you come back to it.
- Backups needs Docker, and runs `pg_dump` from the image matching the server, so nothing has to be installed. Files go in `~/pg-backups`; anything ending `.backup` in there is listed, so older ones can be copied in.
- Open in sandbox restores a backup into a throwaway container, which then appears in the service list. It's a copy of real data, bound to `127.0.0.1`, and stays in Docker until you remove it in Backups.
- Click a result cell to read all of it, format JSON and copy it.
- Export CSV saves to `~/Downloads`, in a read-only transaction: it runs the query again, so one that writes is refused.
- Tabs' SQL, history, the last service and pane sizes are kept in the webview's `localStorage`, so history holds whatever you've run.

## Layout

- `cmd/pgide` — Wails window around `web.Handler`
- `cmd/dev` — same handler on localhost for a normal browser
- `internal/conn` — list services, connect (cancel keeps the connection alive)
- `internal/session` — one connection, run / page / explain / cancel; each query tab has its own
- `internal/dockerpg` — `pg_dump` and sandboxes, through Docker
- `internal/catalog` — its own connection, reads the browser tree, definitions and completions from the system catalogs
- `web` — HTTP handlers, templ views, vendored htmx 2.0.11
- `web/css` — Tailwind source and vendored daisyUI, built into `web/static/app.css`
- `web/js` — editor and tab code, built into `web/static/app.js`
