// Package session runs queries on one dedicated connection and hands results
// back a page at a time, so large result sets never sit in memory.
package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PageSize is the number of rows returned per page.
const PageSize = 500

// ErrStale is returned by More when the query it refers to is no longer the
// current one (a newer query replaced it, or it already finished).
var ErrStale = errors.New("query is no longer running")

// Value is one cell. Values come back in Postgres' text format, which is
// exactly what we want to display.
type Value struct {
	Text string
	Null bool
}

// Page is one chunk of a query's output.
type Page struct {
	QueryID uint64
	Columns []string // set on the first page only
	NumCols int      // set on every page
	Rows    [][]Value
	Total   int // rows returned so far, including this page

	// Set once the query has finished.
	Done    bool
	Tags    []string // command tags, e.g. "SELECT 12", "UPDATE 3"
	Err     error
	Elapsed time.Duration
}

// Session owns one connection. Run, More and Close serialise on mu;
// Cancel deliberately does not, so it can interrupt a Run or More that is
// blocked waiting on the server.
type Session struct {
	connect func(context.Context) (*pgconn.PgConn, error)

	mu   sync.Mutex
	conn *pgconn.PgConn // guarded by mu
	q    *query         // guarded by mu; the current unfinished query

	cancelMu sync.Mutex
	cancel   context.CancelFunc // cancels the most recent query

	nextID atomic.Uint64
}

type query struct {
	id     uint64
	mrr    *pgconn.MultiResultReader
	rr     *pgconn.ResultReader // the result set being paged, once found
	cancel context.CancelFunc
	start  time.Time
	ncols  int
	total  int
	tags   []string
}

// New returns a session using conn. connect is used to reconnect if the
// connection is lost.
func New(conn *pgconn.PgConn, connect func(context.Context) (*pgconn.PgConn, error)) *Session {
	return &Session{conn: conn, connect: connect}
}

// Run cancels whatever is running, starts sql and returns its first page.
//
// sql may hold several statements. The first one that returns columns is
// shown as the grid; the others only contribute their command tags.
func (s *Session) Run(sql string) Page {
	s.Cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeQuery()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancelMu.Lock()
	s.cancel = cancel
	s.cancelMu.Unlock()

	q := &query{id: s.nextID.Add(1), cancel: cancel, start: time.Now()}

	if s.conn == nil || s.conn.IsClosed() {
		conn, err := s.connect(ctx)
		if err != nil {
			cancel()
			return Page{QueryID: q.id, Done: true, Err: err}
		}
		s.conn = conn
	}

	q.mrr = s.conn.Exec(ctx, sql)
	s.q = q
	return s.readPage(q)
}

// More returns the next page of query id.
func (s *Session) More(id uint64) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.q == nil || s.q.id != id {
		return Page{}, ErrStale
	}
	return s.readPage(s.q), nil
}

// Cancel asks the server to stop the running query, if any.
func (s *Session) Cancel() {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

// Close cancels any running query and closes the connection.
func (s *Session) Close(ctx context.Context) error {
	s.Cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeQuery()
	if s.conn == nil {
		return nil
	}
	return s.conn.Close(ctx)
}

// readPage must be called with mu held.
func (s *Session) readPage(q *query) Page {
	p := Page{QueryID: q.id}

	// Skip statements that return no columns (SET, UPDATE, ...) until we
	// reach one we can show as a grid.
	for q.rr == nil {
		if !q.mrr.NextResult() {
			return s.finish(q, p)
		}
		rr := q.mrr.ResultReader()
		if len(rr.FieldDescriptions()) == 0 {
			if tag, err := rr.Close(); err == nil {
				q.tags = append(q.tags, tag.String())
			}
			continue
		}
		q.rr = rr
		fields := rr.FieldDescriptions()
		p.Columns = make([]string, len(fields))
		for i, f := range fields {
			p.Columns[i] = f.Name
		}
		q.ncols = len(fields)
	}
	p.NumCols = q.ncols

	p.Rows = make([][]Value, 0, PageSize)
	for len(p.Rows) < PageSize && q.rr.NextRow() {
		p.Rows = append(p.Rows, copyRow(q.rr.Values()))
	}
	q.total += len(p.Rows)
	p.Total = q.total
	if len(p.Rows) == PageSize {
		return p // probably more to come
	}

	if tag, err := q.rr.Close(); err == nil {
		q.tags = append(q.tags, tag.String())
	}
	return s.finish(q, p)
}

// finish drains the remaining statements, records the outcome on p and
// clears the current query. Must be called with mu held.
func (s *Session) finish(q *query, p Page) Page {
	for q.mrr.NextResult() {
		if tag, err := q.mrr.ResultReader().Close(); err == nil {
			q.tags = append(q.tags, tag.String())
		}
	}
	p.Err = q.mrr.Close()
	p.Done = true
	p.Tags = q.tags
	p.Total = q.total
	p.NumCols = q.ncols
	p.Elapsed = time.Since(q.start)
	q.cancel()
	if s.q == q {
		s.q = nil
	}
	return p
}

// closeQuery abandons the current query. Must be called with mu held, and
// after Cancel so draining the rest of the result is quick.
func (s *Session) closeQuery() {
	if s.q == nil {
		return
	}
	_ = s.q.mrr.Close()
	s.q.cancel()
	s.q = nil
}

// copyRow copies a row out of pgconn's buffer, which is reused on the next
// NextRow call.
func copyRow(vals [][]byte) []Value {
	row := make([]Value, len(vals))
	for i, v := range vals {
		if v == nil {
			row[i] = Value{Null: true}
		} else {
			row[i] = Value{Text: string(v)}
		}
	}
	return row
}
