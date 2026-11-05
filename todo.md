# Todo

Most valuable first within each section.

## Lock in what's there

- [ ] Make the first commit. The repo has no commits, so none of this work is saved in git yet.
- [ ] Check the Wails window: run `go run ./cmd/pgide` and look at the tree, the theme toggle, and whether the saved theme survives a restart.
- [ ] Update the templ CLI to v0.3.1100 to match `go.mod`.
- [ ] Add a `closed` flag to `Catalog` so a tree request in flight during a service switch can't reopen a connection. `Session` needs the same fix.

## Fix rough edges

- [ ] Stop tree clicks losing SQL: one click overwrites the editor. Insert at the cursor when the editor has text, or solve it with query tabs.
- [ ] Hide extension-owned functions by filtering through `pg_depend`.
- [ ] Fix the elapsed time: it runs until the last page, so it includes time spent scrolling. Record it at the first page, or show both.

## Next features

- [ ] Query tabs: several editors, each with its own result. Largest change here, because `Session` is built around one current query.
- [ ] Object details on click: function definitions with `pg_get_functiondef`, and DDL for tables and views, like pgAdmin's SQL tab. Do this before query tabs, as it's much smaller and finishes the tree.
- [ ] Tree search: a filter box, plus group counts such as "Tables (167)".
- [ ] Syntax highlighting and autocomplete in the editor. Needs a JS editor dependency, so decide first.
- [ ] `EXPLAIN` view. another project's `support/indexcheck` is a good starting point.

## Smaller items

- [ ] Add a favicon to clear the 404.
- [ ] Add a read-only mode per connection, as a safety net for production services. Moves up the list if pg will point at anything live.
