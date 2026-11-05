# Todo

Most valuable first within each section.

## Lock in what's there

- [x] Make the first commit. The repo has no commits, so none of this work is saved in git yet.
- [x] Check the Wails window: run `go run ./cmd/pgide` and look at the tree, the theme toggle, and whether the saved theme survives a restart.
- [x] Update the templ CLI to v0.3.1100 to match `go.mod`.
- [x] Add a `closed` flag to `Catalog` so a tree request in flight during a service switch can't reopen a connection. `Session` needs the same fix.

## Fix rough edges

- [x] Stop tree clicks losing SQL: one click overwrites the editor. Insert at the cursor when the editor has text, or solve it with query tabs.
- [x] Hide extension-owned functions by filtering through `pg_depend`.
- [x] Fix the elapsed time: it runs until the last page, so it includes time spent scrolling. Record it at the first page, or show both.

## Next features

- [x] Query tabs: several editors, each with its own result. Largest change here, because `Session` is built around one current query.
- [x] Object details on click: function definitions with `pg_get_functiondef`, and DDL for tables and views, like pgAdmin's SQL tab. Do this before query tabs, as it's much smaller and finishes the tree.
- [x] Tree search: a filter box, plus group counts such as "Tables (167)".
- [x] Syntax highlighting and autocomplete in the editor. Needs a JS editor dependency, so decide first.
- [x] `EXPLAIN` view. Spectra's `support/indexcheck` is a good starting point.

## Smaller items

- [x] Add a favicon to clear the 404.
- [x] Add a read-only mode per connection, as a safety net for production services. Moves up the list if pg will point at anything live.

## Next up

Kept small on purpose: only build these once they're missed in real use.

### Protect what's there

- [x] Add cross-site protection to `cmd/dev`: any website open in the browser can POST SQL to `/query`. Try `http.CrossOriginProtection`, and check it against Wails first.
- [x] Keep SQL across reloads and restarts: save each tab's text in `localStorage`, as closing or reloading discards it today.
- [x] Warn before closing a tab that has SQL in it. Check `confirm()` works in the webview, or use a daisyUI modal.
- [x] Ask before running `update` or `delete` with no `where`.

### Working with results

- [ ] See a cell's full value: results cut off at 40 characters, with no way to read the rest. Click a cell to open it, with JSON formatted.
- [ ] Copy a cell or row to the clipboard.
- [ ] Export a result as CSV, streamed from the session so large results stay out of memory.
- [ ] Show a Running timer, so a slow query can be told from a stuck one.

### Editor

- [ ] Run only the selected text, when there is a selection.
- [ ] Query history: the last statements run, searchable, stored locally.
- [x] Use the connection's `search_path` for autocomplete, rather than assuming `public`.
- [ ] Format SQL. Needs a formatter dependency, so decide first.

### Tree

- [x] Add triggers to the tree and the table DDL, which leaves them out today.
- [x] Show partitions under their parent table, as pgAdmin does.
- [x] Show row counts, using the planner's estimate so it stays fast.
- [x] Let a level that failed to load retry on reopen, rather than needing ↻.

### App

- [ ] Reconnect to the last service on launch.
- [ ] Remember the sidebar width and editor height.
- [x] `EXPLAIN ANALYZE`, in a rolled back transaction, and only with a clear warning that it runs the query.
