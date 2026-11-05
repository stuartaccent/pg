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

Styling is daisyUI 5.7.46 (vendored as `web/css/daisyui.mjs`) on the Tailwind standalone CLI. `web/static/app.css` is generated, so edit `web/css/app.css` instead. The CLI isn't kept in the repo; on Apple silicon, fetch it once with:

```sh
mkdir -p tmp && curl -fsSLo tmp/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/v4.3.3/tailwindcss-macos-arm64 && chmod +x tmp/tailwindcss
```

## Test

The session tests need a real server and are skipped otherwise:

```sh
PGIDE_TEST_SERVICE=local go test -race ./...
```

## Layout

- `cmd/pgide` — Wails window around `web.Handler`
- `cmd/dev` — same handler on localhost for a normal browser
- `internal/conn` — list services, connect (cancel keeps the connection alive)
- `internal/session` — one connection, run / page / cancel
- `internal/catalog` — a second connection, reads the browser tree from the system catalogs
- `web` — HTTP handlers, templ views, vendored htmx 2.0.11
- `web/css` — Tailwind source and vendored daisyUI, built into `web/static/app.css`
