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
- [x] `EXPLAIN` view. another project's `support/indexcheck` is a good starting point.

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

- [x] See a cell's full value: results cut off at 40 characters, with no way to read the rest. Click a cell to open it, with JSON formatted.
- [x] Copy a cell or row to the clipboard.
- [x] Export a result as CSV, streamed from the session so large results stay out of memory.
- [x] Show a Running timer, so a slow query can be told from a stuck one.

### Editor

- [x] Run only the selected text, when there is a selection.
- [x] Query history: the last statements run, searchable, stored locally.
- [x] Use the connection's `search_path` for autocomplete, rather than assuming `public`.
- [x] Format SQL. Needs a formatter dependency, so decide first.

### Tree

- [x] Add triggers to the tree and the table DDL, which leaves them out today.
- [x] Show partitions under their parent table, as pgAdmin does.
- [x] Show row counts, using the planner's estimate so it stays fast.
- [x] Let a level that failed to load retry on reopen, rather than needing ↻.

### App

- [x] Reconnect to the last service on launch.
- [x] Remember the sidebar width and editor height.
- [x] `EXPLAIN ANALYZE`, in a rolled back transaction, and only with a clear warning that it runs the query.

## Also done

Built along the way, outside the lists above.

### Look and feel

- [x] Restyle with daisyUI on Tailwind, with a light and dark theme toggle.
- [x] Drag bars to resize the editor and sidebar, with arrow keys too.
- [x] Close all tabs.
- [x] App icon in Menlo Bold on the dark theme's colours, with a matching favicon.
- [x] A proper `.app` bundle, built with `scripts/bundle.sh`.
- [x] Tidy the Backups dialog: lists line up with the title, no row hover, no focus ring on opening.

### Tree

- [x] Browser tree of the connected database, loading each level on expand, on its own connection.
- [x] Trigger functions in their own group, as pgAdmin does.
- [x] Collations, domains and types, each opening its definition.

### Editor

- [x] Autocomplete column names without a table prefix, from the tables in the statement under the cursor.
- [x] Keep 2,000 statements of history, up to about 2MB, so it can't crowd out tabs' SQL.

### Server

- [x] Activity dialog: every connection on the server, refreshing every 2 seconds, showing what's blocking what, with Cancel and Terminate.
- [x] Roles in the tree: memberships, grants in this database, and a definition without the password.

### Saved queries

- [x] Saved queries as `.sql` files in `~/pg-queries`, opened in a tab without running, with starters for roles and stats.

### Connections

- [x] Config menu to open the service and password files in a text editor.
- [x] Hint about `.pgpass` when a password is rejected.
- [x] Warn when the password file is readable by others.
- [x] Refresh the service list on coming back to the window.
- [x] Let a failed connect be retried from the dropdown.

### Backups

- [x] Back up through Docker, using the image matching the server.
- [x] Sandboxes: restore a backup into a throwaway container and connect to it, with a badge to show it's not the real thing.
- [x] Link to open the backups folder in Finder.
- [x] Verify a backup: restore it into a throwaway container, check every table arrived, and compare row counts.
- [x] Delete backups from the Backups dialog, for good rather than to the Trash.
- [x] Restore a backup into a new database, removing it again if anything goes wrong, with a Connect button when done.
- [x] Replace a database from a backup: restored and checked beside it, then swapped by renaming, keeping the old one to go back to.

### Fixes

- [x] Stop WebKit reusing responses, which left the tree empty in the desktop app.
- [x] Audit: a late Stop reporting failure, leftovers on quit, unread results holding locks, unbounded pages, and failing after a lost connection, which now also reports a lost transaction.
