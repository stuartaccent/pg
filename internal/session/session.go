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

	"pg/internal/conn"
)

// PageSize is the number of rows returned per page.
const PageSize = 500

// ErrStale is returned by More when the query it refers to is no longer the
// current one (a newer query replaced it, or it already finished).
var ErrStale = errors.New("query is no longer running")

// ErrOpenTransaction is returned by an Explain that would have to end the
// transaction the session is in.
var ErrOpenTransaction = errors.New("finish the open transaction first: explain analyze runs in one of its own and rolls it back")

// ErrTransactionLost is returned once when the connection drops during a
// transaction. The server has rolled it back, and the session starts again.
var ErrTransactionLost = errors.New("the connection was lost, and the open transaction with it: nothing in it was committed")

// ErrClosed is returned once the session has been closed.
var ErrClosed = errors.New("session is closed")

// nextID is shared so a query id is never reused by a later session, where an
// old result's sentinel row could otherwise fetch a new query's rows.
var nextID atomic.Uint64

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
	Elapsed time.Duration // time spent running, not waiting for the next page to be asked for
}

// Session owns one connection. Run, More and Close serialise on mu;
// Cancel deliberately does not, so it can interrupt a Run or More that is
// blocked waiting on the server.
type Session struct {
	connect func(context.Context) (*pgconn.PgConn, error)

	mu     sync.Mutex
	conn   *pgconn.PgConn // guarded by mu
	q      *query         // guarded by mu; the current unfinished query
	closed bool           // guarded by mu

	cancelMu sync.Mutex
	cancel   context.CancelFunc // cancels the most recent query
}

type query struct {
	id      uint64
	mrr     *pgconn.MultiResultReader
	rr      *pgconn.ResultReader // the result set being paged, once found
	cancel  context.CancelFunc
	start   time.Time     // when the current page began
	elapsed time.Duration // time spent on earlier pages
	ncols   int
	total   int
	tags    []string
}

// New returns a session using conn, which may be nil to connect on first use.
// connect is used for that and to reconnect if the connection is lost.
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
	ctx, q, err := s.begin()
	if err != nil {
		return Page{QueryID: q.id, Done: true, Err: err}
	}

	q.mrr = s.conn.Exec(ctx, sql)
	s.q = q
	return s.readPage(q)
}

// Explain cancels whatever is running and returns sql's plan. With analyze it
// also runs sql, inside a transaction that is rolled back.
//
// It uses the extended protocol, which refuses more than one statement. Over
// Run, "select 1; delete from t" would explain the select and run the delete.
func (s *Session) Explain(sql string, analyze bool) Page {
	s.Cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, q, err := s.begin()
	if err != nil {
		return Page{QueryID: q.id, Done: true, Err: err}
	}
	defer q.cancel()

	stmt := "explain " + sql
	if analyze {
		// Rolling back would also undo whatever the user has open.
		if s.conn.TxStatus() != 'I' {
			return Page{QueryID: q.id, Done: true, Err: ErrOpenTransaction}
		}
		if _, err := s.conn.Exec(ctx, "begin").ReadAll(); err != nil {
			return Page{QueryID: q.id, Done: true, Err: err}
		}
		defer s.rollback()
		stmt = "explain analyze " + sql
	}

	res := s.conn.ExecParams(ctx, stmt, nil, nil, nil, nil).Read()
	p := Page{QueryID: q.id, Done: true, Err: res.Err, Elapsed: time.Since(q.start)}
	if res.Err != nil {
		return p
	}
	p.Columns, p.NumCols = []string{"QUERY PLAN"}, 1
	p.Rows = make([][]Value, len(res.Rows))
	for i, row := range res.Rows {
		p.Rows[i] = copyRow(row)
	}
	p.Total = len(p.Rows)
	return p
}

// rollback ends the current transaction, even if the query's context was
// cancelled. Failing to is unsafe to carry on from, so it drops the connection.
func (s *Session) rollback() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.conn.Exec(ctx, "rollback").ReadAll(); err != nil {
		_ = s.conn.Close(ctx)
	}
}

// begin abandons the current query and readies the connection for a new one.
// Must be called with mu held, and after Cancel.
func (s *Session) begin() (context.Context, *query, error) {
	s.closeQuery()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancelMu.Lock()
	s.cancel = cancel
	s.cancelMu.Unlock()

	q := &query{id: nextID.Add(1), cancel: cancel, start: time.Now()}
	if s.closed {
		cancel()
		return ctx, q, ErrClosed
	}
	if s.conn != nil && !conn.Alive(ctx, s.conn) {
		inTransaction := s.conn.TxStatus() != 'I'
		_ = s.conn.Close(ctx)
		s.conn = nil
		// Reconnecting quietly here would run what comes next outside the
		// transaction, where it would commit by itself.
		if inTransaction {
			cancel()
			return ctx, q, ErrTransactionLost
		}
	}
	if s.conn == nil {
		fresh, err := s.connect(ctx)
		if err != nil {
			cancel()
			return ctx, q, err
		}
		s.conn = fresh
	}
	return ctx, q, nil
}

// More returns the next page of query id.
func (s *Session) More(id uint64) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.q == nil || s.q.id != id {
		return Page{}, ErrStale
	}
	s.q.start = time.Now()
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
	s.closed = true
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
		q.elapsed += time.Since(q.start)
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
	p.Elapsed = q.elapsed + time.Since(q.start)
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
