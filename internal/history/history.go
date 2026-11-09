// Package history keeps every query run, in SQLite, grouped by what the query
// is rather than how it was typed.
package history

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	_ "modernc.org/sqlite"
)

// ShowMost is the most entries List returns.
const ShowMost = 500

// RecentRuns is how many runs Detail returns. Entry.Runs counts them all; past
// the last few, the times and places add nothing.
const RecentRuns = 5

const schema = `
create table if not exists queries (
	id      integer primary key,
	key     text not null unique, -- the fingerprint, or the text if it doesn't parse
	sql     text not null,        -- as last typed
	starred integer not null default 0,
	note    text not null default ''
);
create table if not exists runs (
	query_id integer not null references queries(id),
	ran_at   integer not null, -- unix milliseconds
	service  text not null,
	database text not null
);
create index if not exists runs_query on runs(query_id);
create index if not exists runs_ran_at on runs(ran_at);
`

// Store is one history database.
type Store struct{ db *sql.DB }

// Entry is one query and a summary of its runs.
type Entry struct {
	ID       int64
	SQL      string
	Note     string
	Starred  bool
	LastRun  time.Time // zero if its runs were cleared, which only a starred one outlives
	Runs     int
	Services string // the services it ran on, comma separated
}

// Run is one time a query ran.
type Run struct {
	At       time.Time
	Service  string
	Database string
}

// Filter narrows List. Sort is "runs", "sql", or anything else for the most
// recently run first. Starred ones always come first.
type Filter struct {
	Search  string
	Service string
	Starred bool
	Sort    string
}

// Path is where the history lives: ~/Library/Application Support/pg on macOS.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "pg", "history.db"), err
}

// Open opens the history at path, making it if need be. Each window is its own
// process, so writers wait for each other rather than fail.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Record adds a run of a query. typed is the SQL as typed, which is what's
// kept, so running it again asks for any :params again. filled has the
// params' values, so it parses and gives the fingerprint.
func (s *Store) Record(ctx context.Context, typed, filled, service, database string, at time.Time) error {
	key, err := pg_query.Fingerprint(filled)
	if err != nil {
		key = "text:" + typed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `insert into queries (key, sql) values (?, ?)
		on conflict (key) do update set sql = excluded.sql returning id`, key, typed).Scan(&id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "insert into runs values (?, ?, ?, ?)", id, at.UnixMilli(), service, database); err != nil {
		return err
	}
	return tx.Commit()
}

const entryColumns = `select q.id, q.sql, q.note, q.starred, coalesce(max(r.ran_at), 0), count(r.query_id), coalesce(group_concat(distinct r.service), '')
	from queries q left join runs r on r.query_id = q.id`

func scanEntry(row interface{ Scan(...any) error }) (Entry, error) {
	var e Entry
	var lastRun int64
	err := row.Scan(&e.ID, &e.SQL, &e.Note, &e.Starred, &lastRun, &e.Runs, &e.Services)
	if lastRun != 0 {
		e.LastRun = time.UnixMilli(lastRun)
	}
	return e, err
}

// List returns the entries that match f, at most ShowMost. With a service,
// the run count and last run are of the runs on that service.
func (s *Store) List(ctx context.Context, f Filter) ([]Entry, error) {
	order := "max(r.ran_at) desc"
	switch f.Sort {
	case "runs":
		order = "count(r.query_id) desc, max(r.ran_at) desc"
	case "sql":
		order = "q.sql collate nocase"
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(f.Search) + "%"
	rows, err := s.db.QueryContext(ctx, entryColumns+`
		where (?1 = '' or r.service = ?1)
		and (?2 = 0 or q.starred)
		and (q.sql like ?3 escape '\' or q.note like ?3 escape '\')
		group by q.id
		order by q.starred desc, `+order+`
		limit ?4`, f.Service, f.Starred, like, ShowMost)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ErrNotFound is returned by Detail for an entry that has gone.
var ErrNotFound = errors.New("no longer in history")

// Detail returns an entry and its RecentRuns newest runs, newest first.
func (s *Store) Detail(ctx context.Context, id int64) (Entry, []Run, error) {
	e, err := scanEntry(s.db.QueryRowContext(ctx, entryColumns+" where q.id = ? group by q.id", id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, nil, ErrNotFound
	}
	if err != nil {
		return e, nil, err
	}
	rows, err := s.db.QueryContext(ctx, "select ran_at, service, database from runs where query_id = ? order by ran_at desc limit ?", id, RecentRuns)
	if err != nil {
		return e, nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var r Run
		var at int64
		if err := rows.Scan(&at, &r.Service, &r.Database); err != nil {
			return e, nil, err
		}
		r.At = time.UnixMilli(at)
		runs = append(runs, r)
	}
	return e, runs, rows.Err()
}

// Services lists every service a query has run on, for filtering by.
func (s *Store) Services(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "select distinct service from runs order by service")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var services []string
	for rows.Next() {
		var service string
		if err := rows.Scan(&service); err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, rows.Err()
}

// Star stars or unstars an entry. Starred ones come first, and clearing
// leaves them.
func (s *Store) Star(ctx context.Context, id int64, starred bool) error {
	_, err := s.db.ExecContext(ctx, "update queries set starred = ? where id = ?", starred, id)
	return err
}

func (s *Store) SetNote(ctx context.Context, id int64, note string) error {
	_, err := s.db.ExecContext(ctx, "update queries set note = ? where id = ?", note, id)
	return err
}

// Remove deletes an entry and its runs.
func (s *Store) Remove(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "delete from runs where query_id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "delete from queries where id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearBefore deletes runs before t, and the entries left with none, apart
// from starred ones.
func (s *Store) ClearBefore(ctx context.Context, t time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "delete from runs where ran_at < ?", t.UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "delete from queries where not starred and id not in (select query_id from runs)"); err != nil {
		return err
	}
	return tx.Commit()
}
