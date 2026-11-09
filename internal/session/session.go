// Package session runs queries on one dedicated connection and hands results
// back a page at a time, so large result sets never sit in memory.
package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"pg/internal/conn"
)

const (
	// PageSize is the most rows returned per page.
	PageSize = 500
	// maxPageBytes also ends a page, as a few hundred rows of large values could
	// otherwise be gigabytes by the time they reach the window.
	maxPageBytes = 8 << 20
	// idleLimit is how long an unfinished query waits for its next page.
	idleLimit = 5 * time.Minute
)

// copiesToClient reports whether sql has a COPY to or from the client. Nothing
// here can feed one, so the server would wait for ever, and nothing can show
// what one sends. SQL that doesn't parse is left for the server to reject.
func copiesToClient(sql string) bool {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return false
	}
	for _, stmt := range tree.Stmts {
		if c := stmt.Stmt.GetCopyStmt(); c != nil && c.Filename == "" && !c.IsProgram {
			return true
		}
	}
	return false
}

// ErrCopy is why a COPY to or from the client isn't run.
var ErrCopy = errors.New("pg can't run COPY to or from the client: use a file on the server, or Export CSV")

// ErrOneStatement is returned by an Explain given more than one statement.
var ErrOneStatement = errors.New("explain takes one statement at a time: select just the one to explain")

// ErrExpired ends a result whose next page wasn't asked for in time.
var ErrExpired = errors.New("stopped after five minutes without scrolling, so it wouldn't keep holding locks: run it again for the rest")

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

// Source is the table column a result column came from, or zeros if it
// didn't come straight from one.
type Source struct {
	Table uint32 // pg_class oid
	Attr  uint16 // attnum
}

// Page is one chunk of a query's output.
type Page struct {
	QueryID uint64
	Columns []string // set on the first page only
	Sources []Source // where each column came from, set with Columns
	NumCols int      // set on every page
	Rows    [][]Value
	Total   int // rows returned so far, including this page

	// Plan is what Explain returns instead of rows: the plan as JSON.
	Plan string

	// Ended is set on a result's last page when another result follows, which
	// More returns next, with its own Columns.
	Ended bool

	// Set once the query has finished, and Tags and Elapsed also when Ended.
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

	mu      sync.Mutex
	conn    *pgconn.PgConn // guarded by mu
	q       *query         // guarded by mu; the current unfinished query
	closed  bool           // guarded by mu
	expired *Page          // guarded by mu; how the last query to expire ended
	idle    time.Duration  // how long a query waits for its next page

	cancelMu sync.Mutex
	cancel   context.CancelFunc // cancels the most recent query
}

type query struct {
	id      uint64
	timer   *time.Timer // ends it if the next page isn't asked for
	mrr     *pgconn.MultiResultReader
	rr      *pgconn.ResultReader // the result set being paged, once found
	ran     int                  // statements finished so far
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
	return &Session{conn: conn, connect: connect, idle: idleLimit}
}

// Run cancels whatever is running, starts sql and returns its first page.
//
// sql may hold several statements. Each one that returns columns is a result
// of its own, see Page.Ended; the others only contribute their command tags.
func (s *Session) Run(sql string) Page {
	s.Cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, q, err := s.begin()
	if err != nil {
		return Page{QueryID: q.id, Done: true, Err: err}
	}
	if copiesToClient(sql) {
		q.cancel()
		return Page{QueryID: q.id, Done: true, Err: ErrCopy}
	}

	q.mrr = s.conn.Exec(ctx, sql)
	s.q = q
	return s.readPage(q)
}

// Explain cancels whatever is running and returns sql's plan in Page.Plan. With
// analyze it also runs sql, inside a transaction that is rolled back.
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
	// Postgres would refuse these too, but less helpfully.
	if stmts, err := pg_query.SplitWithScanner(sql, true); err == nil && len(stmts) > 1 {
		return Page{QueryID: q.id, Done: true, Err: ErrOneStatement}
	}

	stmt := "explain (format json) " + sql
	if analyze {
		// Rolling back would also undo whatever the user has open.
		if s.conn.TxStatus() != 'I' {
			return Page{QueryID: q.id, Done: true, Err: ErrOpenTransaction}
		}
		if _, err := s.conn.Exec(ctx, "begin").ReadAll(); err != nil {
			return Page{QueryID: q.id, Done: true, Err: err}
		}
		defer s.rollback()
		stmt = "explain (analyze, buffers, format json) " + sql
	}

	res := s.conn.ExecParams(ctx, stmt, nil, nil, nil, nil).Read()
	p := Page{QueryID: q.id, Done: true, Err: res.Err, Elapsed: time.Since(q.start)}
	if res.Err != nil {
		return p
	}
	p.Plan = string(res.Rows[0][0])
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

	q := &query{id: nextID.Add(1), cancel: cancel}
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
	// Timed from here, so the check above doesn't count: on a distant server,
	// it would double the time a quick query seems to take.
	q.start = time.Now()
	return ctx, q, nil
}

// More returns the next page of query id.
func (s *Session) More(id uint64) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expired != nil && s.expired.QueryID == id {
		return *s.expired, nil
	}
	if s.q == nil || s.q.id != id {
		return Page{}, ErrStale
	}
	s.q.timer.Stop()
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

	if q.rr == nil && !q.nextResult() {
		return s.finish(q, p)
	}
	if q.ncols == 0 {
		fields := q.rr.FieldDescriptions()
		p.Columns = make([]string, len(fields))
		p.Sources = make([]Source, len(fields))
		for i, f := range fields {
			p.Columns[i] = f.Name
			p.Sources[i] = Source{f.TableOID, f.TableAttributeNumber}
		}
		q.ncols = len(fields)
	}
	p.NumCols = q.ncols

	p.Rows = make([][]Value, 0, PageSize)
	size := 0
	more := true
	for len(p.Rows) < PageSize && size < maxPageBytes {
		if more = q.rr.NextRow(); !more {
			break
		}
		values := q.rr.Values()
		for _, v := range values {
			size += len(v)
		}
		p.Rows = append(p.Rows, copyRow(values))
	}
	q.total += len(p.Rows)
	p.Total = q.total
	if !more {
		q.closeResult(q.rr)
		if !q.nextResult() {
			return s.finish(q, p)
		}
		p.Ended, p.Tags, p.Elapsed = true, q.tags, q.elapsed+time.Since(q.start)
		q.tags, q.ncols, q.total = nil, 0, 0
	}
	q.elapsed += time.Since(q.start)
	// An unread result keeps the query running on the server, with its locks,
	// so it only gets so long.
	q.timer = time.AfterFunc(s.idle, func() { s.expire(q) })
	return p // probably more to come
}

// nextResult moves on to the next statement that returns columns, reporting
// whether there was one. Those that don't (SET, UPDATE, ...) leave their tags.
func (q *query) nextResult() bool {
	for q.mrr.NextResult() {
		rr := q.mrr.ResultReader()
		if len(rr.FieldDescriptions()) > 0 {
			q.rr = rr
			return true
		}
		q.closeResult(rr)
	}
	return false
}

func (q *query) closeResult(rr *pgconn.ResultReader) {
	if tag, err := rr.Close(); err == nil {
		q.tags = append(q.tags, tag.String())
		q.ran++
	}
}

// expire ends q if it's still waiting for its next page.
func (s *Session) expire(q *query) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.q != q {
		return
	}
	q.cancel()
	s.closeQuery()
	s.expired = &Page{QueryID: q.id, NumCols: q.ncols, Total: q.total, Done: true, Err: ErrExpired}
}

// finish records the outcome on p and clears the current query. Must be called
// with mu held.
func (s *Session) finish(q *query, p Page) Page {
	p.Err = q.mrr.Close()
	// Statements sent together are one transaction, so results already on
	// screen can look like they took effect.
	if p.Err != nil && q.ran > 0 && s.conn.TxStatus() == 'I' {
		p.Err = fmt.Errorf("%w\nThe statements before it were rolled back too, unless the script committed them", p.Err)
	}
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
	if s.q.timer != nil {
		s.q.timer.Stop()
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
