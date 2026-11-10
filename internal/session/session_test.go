package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
)

// These tests need a real server. Set PGIDE_TEST_SERVICE to a service name
// from your pg_service.conf; they only run read-only statements and temp tables.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	service := os.Getenv("PGIDE_TEST_SERVICE")
	if service == "" {
		t.Skip("PGIDE_TEST_SERVICE not set")
	}
	connect := func(ctx context.Context) (*pgconn.PgConn, error) { return conn.Open(ctx, conn.ServiceDSN(service)) }
	c, err := connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := New(c, connect)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestRun(t *testing.T) {
	s := newTestSession(t)

	tests := []struct {
		name      string
		sql       string
		wantCols  []string
		wantRows  int
		wantTags  []string
		wantErr   string
		checkNull bool
	}{
		{name: "select", sql: "select 1 as a, null as b", wantCols: []string{"a", "b"}, wantRows: 1, wantTags: []string{"SELECT 1"}, checkNull: true},
		{name: "command only", sql: "set search_path = public", wantTags: []string{"SET"}},
		{name: "command then select", sql: "set search_path = public; select 2 as x", wantCols: []string{"x"}, wantRows: 1, wantTags: []string{"SET", "SELECT 1"}},
		{name: "syntax error", sql: "selec 1", wantErr: "syntax error"},
		{name: "error after rows", sql: "select 1/(2-g) from generate_series(1,3) g", wantCols: []string{"?column?"}, wantErr: "division by zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := s.Run(tt.sql)
			if !p.Done {
				t.Fatal("expected a single-page result")
			}
			if tt.wantErr != "" {
				if p.Err == nil || !strings.Contains(p.Err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", p.Err, tt.wantErr)
				}
				return
			}
			if p.Err != nil {
				t.Fatal(p.Err)
			}
			if strings.Join(p.Columns, ",") != strings.Join(tt.wantCols, ",") {
				t.Errorf("columns = %v, want %v", p.Columns, tt.wantCols)
			}
			if len(p.Rows) != tt.wantRows || p.Total != tt.wantRows {
				t.Errorf("rows = %d (total %d), want %d", len(p.Rows), p.Total, tt.wantRows)
			}
			if strings.Join(p.Tags, ",") != strings.Join(tt.wantTags, ",") {
				t.Errorf("tags = %v, want %v", p.Tags, tt.wantTags)
			}
			if tt.checkNull && (p.Rows[0][0] != Value{Text: "1"} || !p.Rows[0][1].Null) {
				t.Errorf("row = %+v, want [1 NULL]", p.Rows[0])
			}
		})
	}
}

func TestSources(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("create temp table src(id int primary key, a text); select a, 1 from src")
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	if len(p.Sources) != 2 || p.Sources[0].Table == 0 || p.Sources[0].Attr != 2 || p.Sources[1] != (Source{}) {
		t.Fatalf("sources = %+v", p.Sources)
	}
}

func TestEachStatementGetsAResult(t *testing.T) {
	s := newTestSession(t)

	p := s.Run("set search_path = public; select 1 as a; set search_path = public; select 2 as b, 3 as c; set search_path = public")
	if p.Done || !p.Ended || p.Columns[0] != "a" || p.Total != 1 {
		t.Fatalf("first page = %+v", p)
	}
	if got := strings.Join(p.Tags, ","); got != "SET,SELECT 1,SET" {
		t.Errorf("first tags = %s", got)
	}
	p, err := s.More(p.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Done || p.Ended || p.NumCols != 2 || p.Columns[1] != "c" || p.Total != 1 || p.Rows[0][0].Text != "2" {
		t.Fatalf("second page = %+v", p)
	}
	if got := strings.Join(p.Tags, ","); got != "SELECT 1,SET" {
		t.Errorf("second tags = %s", got)
	}
}

func TestLaterResultsArePaged(t *testing.T) {
	s := newTestSession(t)

	p := s.Run(fmt.Sprintf("select generate_series(1, %d); select 'next' as b", PageSize+1))
	if p.Ended || p.Done || p.Total != PageSize {
		t.Fatalf("first page = %+v", p)
	}
	p, err := s.More(p.QueryID)
	if err != nil || !p.Ended || p.Columns != nil || p.Total != PageSize+1 {
		t.Fatalf("second page = %+v, %v", p, err)
	}
	p, err = s.More(p.QueryID)
	if err != nil || !p.Done || p.Columns[0] != "b" || p.Total != 1 {
		t.Fatalf("third page = %+v, %v", p, err)
	}
}

func TestFailingLaterStatementSaysItRolledBack(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("create temp table batch (id int)"); p.Err != nil {
		t.Fatal(p.Err)
	}

	p := s.Run("insert into batch values (1); select 1 as a; select 1/0")
	for !p.Done {
		var err error
		if p, err = s.More(p.QueryID); err != nil {
			t.Fatal(err)
		}
	}
	if p.Err == nil || !strings.Contains(p.Err.Error(), "division by zero") || !strings.Contains(p.Err.Error(), "rolled back") {
		t.Fatalf("err = %v", p.Err)
	}
	if p := s.Run("select count(*) from batch"); p.Rows[0][0].Text != "0" {
		t.Fatalf("not rolled back: %+v", p)
	}

	// One statement failing has nothing before it to mention.
	if p := s.Run("select 1/0"); p.Err == nil || strings.Contains(p.Err.Error(), "rolled back") {
		t.Fatalf("err = %v", p.Err)
	}
	// Nor does an open transaction, which the user still has to end.
	p = s.Run("begin; insert into batch values (1); select 1/0")
	if p.Err == nil || strings.Contains(p.Err.Error(), "rolled back") {
		t.Fatalf("err = %v", p.Err)
	}
	if p := s.Run("rollback"); p.Err != nil {
		t.Fatal(p.Err)
	}
}

func TestPaging(t *testing.T) {
	s := newTestSession(t)
	const n = PageSize*2 + 7

	p := s.Run("select g from generate_series(1, 1007) g")
	total := len(p.Rows)
	for !p.Done {
		var err error
		if p, err = s.More(p.QueryID); err != nil {
			t.Fatal(err)
		}
		total += len(p.Rows)
	}
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	if total != n || p.Total != n {
		t.Fatalf("got %d rows (total %d), want %d", total, p.Total, n)
	}
}

func TestNewRunMakesOldQueryStale(t *testing.T) {
	s := newTestSession(t)
	old := s.Run("select g from generate_series(1, 5000) g")
	if old.Done {
		t.Fatal("expected more pages")
	}
	s.Run("select 1")
	if _, err := s.More(old.QueryID); !errors.Is(err, ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
}

func TestCancelKeepsConnection(t *testing.T) {
	s := newTestSession(t)

	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Cancel()
	}()
	start := time.Now()
	p := s.Run("select pg_sleep(30)")
	if p.Err == nil || !strings.Contains(p.Err.Error(), "canceling statement") {
		t.Fatalf("err = %v, want a cancel error", p.Err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %v", d)
	}

	pid := func() string { return s.Run("select pg_backend_pid()").Rows[0][0].Text }
	before := pid()
	if p := s.Run("select 1"); p.Err != nil {
		t.Fatalf("connection unusable after cancel: %v", p.Err)
	}
	if after := pid(); after != before {
		t.Errorf("backend changed from %s to %s; cancel should not drop the connection", before, after)
	}
}

func TestReconnect(t *testing.T) {
	s := newTestSession(t)
	s.mu.Lock()
	_ = s.conn.Close(context.Background())
	s.mu.Unlock()

	if p := s.Run("select 1"); p.Err != nil {
		t.Fatalf("expected reconnect, got %v", p.Err)
	}
}

func TestElapsedLeavesOutTimeBetweenPages(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("select g from generate_series(1, 600) g")
	if p.Done {
		t.Fatal("expected more than one page")
	}
	pause := 300 * time.Millisecond
	time.Sleep(pause)
	p, err := s.More(p.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Done || p.Elapsed <= 0 || p.Elapsed >= pause {
		t.Fatalf("done = %v, elapsed = %v, want under %v", p.Done, p.Elapsed, pause)
	}
}

func TestExplain(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("create temp table explained (id int); insert into explained values (1)"); p.Err != nil {
		t.Fatal(p.Err)
	}

	p := s.Explain("delete from explained;", false)
	if p.Err != nil || !p.Done || !strings.Contains(p.Plan, `"Node Type"`) {
		t.Fatalf("page = %+v", p)
	}

	// A second statement must be refused, not run.
	if p := s.Explain("select 1; delete from explained", false); !errors.Is(p.Err, ErrOneStatement) {
		t.Fatalf("two statements: err = %v", p.Err)
	}
	p = s.Run("select count(*) from explained")
	if p.Err != nil || p.Rows[0][0].Text != "1" {
		t.Fatalf("explain changed the table: %+v", p)
	}
}

func TestExplainAnalyzeRollsBack(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("create temp table analyzed (id int); insert into analyzed values (1)"); p.Err != nil {
		t.Fatal(p.Err)
	}

	p := s.Explain("delete from analyzed", true)
	if p.Err != nil || !strings.Contains(p.Plan, `"Actual Rows"`) {
		t.Fatalf("page = %+v", p)
	}
	if p := s.Run("select count(*) from analyzed"); p.Err != nil || p.Rows[0][0].Text != "1" {
		t.Fatalf("analyze changed the table: %+v", p)
	}

	// A failure must roll back too, leaving the session usable.
	if p := s.Explain("select nope", true); p.Err == nil {
		t.Fatal("bad sql: want an error")
	}
	if p := s.Run("select 1"); p.Err != nil {
		t.Fatalf("session left in a failed transaction: %v", p.Err)
	}

	// It must not end a transaction the user has open.
	if p := s.Run("begin; delete from analyzed"); p.Err != nil {
		t.Fatal(p.Err)
	}
	if p := s.Explain("select 1", true); !errors.Is(p.Err, ErrOpenTransaction) {
		t.Fatalf("err = %v, want ErrOpenTransaction", p.Err)
	}
	if p := s.Run("select count(*) from analyzed"); p.Err != nil || p.Rows[0][0].Text != "0" {
		t.Fatalf("user's transaction was lost: %+v", p)
	}
	if p := s.Run("rollback"); p.Err != nil {
		t.Fatal(p.Err)
	}
}

func TestClosedSessionDoesNotReconnect(t *testing.T) {
	s := newTestSession(t)
	connects := 0
	connect := s.connect
	s.connect = func(ctx context.Context) (*pgconn.PgConn, error) {
		connects++
		return connect(ctx)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := s.Run("select 1"); !errors.Is(p.Err, ErrClosed) {
		t.Errorf("Run err = %v, want ErrClosed", p.Err)
	}
	if p := s.Explain("select 1", false); !errors.Is(p.Err, ErrClosed) {
		t.Errorf("Explain err = %v, want ErrClosed", p.Err)
	}
	if connects != 0 {
		t.Errorf("reconnected %d times", connects)
	}
}

func TestConnectsOnFirstUse(t *testing.T) {
	s := newTestSession(t)
	lazy := New(nil, s.connect)
	t.Cleanup(func() { _ = lazy.Close(context.Background()) })
	if p := lazy.Run("select 1"); p.Err != nil || len(p.Rows) != 1 {
		t.Fatalf("page = %+v", p)
	}
}

// terminate ends a session's connection from outside, as a server restart would.
func terminate(t *testing.T, s *Session) {
	t.Helper()
	p := s.Run("select pg_backend_pid()")
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	endBackend(t, s, p.Rows[0][0].Text)
}

func endBackend(t *testing.T, s *Session, pid string) {
	t.Helper()
	ctx := context.Background()
	other, err := s.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)
	if _, err := other.Exec(ctx, "select pg_terminate_backend("+pid+")").ReadAll(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
}

func TestReconnectsWithoutFailingTheNextQuery(t *testing.T) {
	s := newTestSession(t)
	terminate(t, s)
	if p := s.Run("select 1"); p.Err != nil || len(p.Rows) != 1 {
		t.Fatalf("page = %+v", p)
	}
}

func TestLosingATransactionIsReported(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("begin; select pg_backend_pid()")
	if p.Err != nil {
		t.Fatal(p.Err)
	}
	endBackend(t, s, p.Rows[0][0].Text)

	// Quietly reconnecting would commit this by itself, outside the transaction.
	if p := s.Run("create temp table lost (id int)"); !errors.Is(p.Err, ErrTransactionLost) {
		t.Fatalf("err = %v, want ErrTransactionLost", p.Err)
	}
	if p := s.Run("select to_regclass('pg_temp.lost') is null"); p.Err != nil || p.Rows[0][0].Text != "t" {
		t.Fatalf("after: %+v", p)
	}
}

// Cancelling while a run checks the connection mustn't close it, and with it
// the open transaction. It's a race, so it's tried a few times.
func TestCancelDuringTheCheckKeepsTheTransaction(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("begin"); p.Err != nil {
		t.Fatal(p.Err)
	}
	for range 40 {
		go s.Cancel()
		if p := s.Run("select 1"); errors.Is(p.Err, ErrTransactionLost) {
			t.Fatal("lost the transaction")
		}
	}
}

func TestEachResultIsTimedOnItsOwn(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("select pg_sleep(0.3); select 1")
	if !p.Ended || p.Elapsed < 300*time.Millisecond {
		t.Fatalf("first = %+v", p)
	}
	p, err := s.More(p.QueryID)
	if err != nil || !p.Done || p.Elapsed >= 300*time.Millisecond {
		t.Fatalf("second = %+v, %v", p, err)
	}
}

// A timer that fired just as More stopped it mustn't end the query More served.
func TestLateTimerIsIgnored(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("select g from generate_series(1, 2000) g")
	q := s.q
	late := q.armed
	if _, err := s.More(p.QueryID); err != nil {
		t.Fatal(err)
	}
	s.expire(q, late)
	if p, err := s.More(p.QueryID); err != nil || p.Err != nil || len(p.Rows) != PageSize {
		t.Fatalf("page = %+v, err = %v", p, err)
	}
}

func TestUnreadResultExpires(t *testing.T) {
	s := newTestSession(t)
	s.idle = 100 * time.Millisecond

	p := s.Run("select g from generate_series(1, 2000) g")
	if p.Done {
		t.Fatal("expected more than one page")
	}
	// Asking in time carries on, and resets the clock.
	time.Sleep(60 * time.Millisecond)
	if p, err := s.More(p.QueryID); err != nil || p.Err != nil || len(p.Rows) != PageSize {
		t.Fatalf("page = %+v, err = %v", p, err)
	}
	time.Sleep(60 * time.Millisecond)
	p, err := s.More(p.QueryID)
	if err != nil || p.Done {
		t.Fatalf("page = %+v, err = %v", p, err)
	}

	time.Sleep(300 * time.Millisecond)
	end, err := s.More(p.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(end.Err, ErrExpired) || !end.Done || end.Total != 3*PageSize || end.NumCols != 1 {
		t.Fatalf("end = %+v", end)
	}
	// The session is fine afterwards, and a finished query never expires.
	if p := s.Run("select 1"); p.Err != nil {
		t.Fatal(p.Err)
	}
	time.Sleep(200 * time.Millisecond)
	if p := s.Run("select 2 as still_here"); p.Err != nil || p.Rows[0][0].Text != "2" {
		t.Fatalf("page = %+v", p)
	}
}

func TestPagesStopAtASize(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("select repeat('x', 1024 * 1024) from generate_series(1, 20)")
	if p.Err != nil || p.Done {
		t.Fatalf("done = %v, err = %v", p.Done, p.Err)
	}
	if len(p.Rows) != 8 {
		t.Errorf("first page has %d rows, want 8", len(p.Rows))
	}
	total := len(p.Rows)
	for !p.Done {
		var err error
		if p, err = s.More(p.QueryID); err != nil {
			t.Fatal(err)
		}
		total += len(p.Rows)
	}
	if total != 20 || p.Total != 20 {
		t.Errorf("got %d rows, Total = %d", total, p.Total)
	}
}

func TestLastPageExactlyFull(t *testing.T) {
	s := newTestSession(t)
	p := s.Run("select g from generate_series(1, 500) g")
	if p.Done {
		t.Fatal("a full page can't know it's the last")
	}
	if p, err := s.More(p.QueryID); err != nil || !p.Done || p.Total != 500 || len(p.Rows) != 0 {
		t.Fatalf("page = %+v, err = %v", p, err)
	}
}

func TestCopyToTheClientIsRefused(t *testing.T) {
	s := newTestSession(t)
	for _, sql := range []string{
		"copy t from stdin",
		"COPY t (a, b) FROM STDIN WITH (FORMAT csv)",
		"copy (select 1) to stdout",
		"select 1; copy t from stdin",
		"/* x */ copy t to stdout",
	} {
		if p := s.Run(sql); !errors.Is(p.Err, ErrCopy) {
			t.Errorf("%q: err = %v", sql, p.Err)
		}
	}
	for _, sql := range []string{"select 'copy to stdout'", "select 1 as copy", "select ';copy t from stdin'"} {
		if p := s.Run(sql); p.Err != nil {
			t.Errorf("%q: %v", sql, p.Err)
		}
	}
}

// Cutting off a result Postgres is still sending rolls back its statement and
// the transaction it's in. The next run says so rather than carrying on.
func TestStoppingAnUnfinishedResultThatUndidWorkSaysSo(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("create temp table undone (id int)"); p.Err != nil {
		t.Fatal(p.Err)
	}
	big := "select generate_series(1, 1000000)"
	count := func() string {
		t.Helper()
		p := s.Run("select count(*) from undone")
		if p.Err != nil {
			t.Fatal(p.Err)
		}
		return p.Rows[0][0].Text
	}

	// In a transaction: the commit after it would have been a rollback.
	if p := s.Run("begin; insert into undone values (1)"); p.Err != nil || !p.InTransaction || !s.InTransaction() {
		t.Fatalf("begin = %+v", p)
	}
	if p := s.Run(big); p.Done {
		t.Fatal("expected more to come")
	}
	if p := s.Run("commit"); !errors.Is(p.Err, ErrInterrupted) {
		t.Fatalf("commit = %v, want ErrInterrupted", p.Err)
	}
	if p := s.Run("rollback"); p.Err != nil || p.InTransaction || s.InTransaction() {
		t.Fatalf("rollback = %+v", p)
	}
	if got := count(); got != "0" {
		t.Fatalf("count = %s", got)
	}

	// In one run with the write before it.
	if p := s.Run("insert into undone values (1); " + big); p.Done {
		t.Fatal("expected more to come")
	}
	if p := s.Run("select 1"); !errors.Is(p.Err, ErrInterrupted) {
		t.Fatalf("after a write = %v, want ErrInterrupted", p.Err)
	}
	if got := count(); got != "0" {
		t.Fatalf("count = %s", got)
	}

	// A plain select undoes nothing, so the next run just runs.
	if p := s.Run(big); p.Done {
		t.Fatal("expected more to come")
	}
	if p := s.Run("select 1"); p.Err != nil {
		t.Fatalf("after a select = %v", p.Err)
	}
}

func TestOnlyReads(t *testing.T) {
	for sql, want := range map[string]bool{
		"select 1":                      true,
		"select 1; select 2":            true,
		"with a as (select 1) select 1": true,
		"insert into t values (1)":      false,
		"select 1 into t":               false,
		"with d as (delete from t returning *) select * from d": false,
		"set work_mem = '1GB'; select 1":                        false,
		"not sql at all":                                        false,
	} {
		if got := onlyReads(sql); got != want {
			t.Errorf("%q: got %v", sql, got)
		}
	}
}

func TestSearchPathIsRemembered(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("select 1"); p.Err != nil || s.SearchPath() != "" {
		t.Fatalf("before = %q, %v", s.SearchPath(), p.Err)
	}
	if p := s.Run("set search_path = pg_catalog, public; select 1"); p.Err != nil {
		t.Fatal(p.Err)
	}
	if got := s.SearchPath(); got != "pg_catalog, public" {
		t.Errorf("search_path = %q", got)
	}
}

func TestCopiesToClient(t *testing.T) {
	for sql, want := range map[string]bool{
		"copy t from stdin":                   true,
		"copy t to stdout":                    true,
		"select 1; copy (select 1) to stdout": true,
		"copy t to '/tmp/x'":                  false,
		"copy t from program 'cat'":           false,
		"select 'copy t to stdout'":           false,
	} {
		if got := copiesToClient(sql); got != want {
			t.Errorf("%q: got %v", sql, got)
		}
	}
}

// Selects cut off undo nothing, however many ran before.
func TestStoppingReadsCarriesOn(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("select 1; select generate_series(1, 1000000)"); p.Done {
		t.Fatal("expected more to come")
	}
	if p := s.Run("select 2"); p.Err != nil {
		t.Fatalf("after reads = %v", p.Err)
	}
}

// A run that opens a transaction and is still paging counts too, though the
// server hasn't said so yet.
func TestUnfinishedWritingRunCountsAsInTransaction(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("create temp table paged (id int)"); p.Err != nil {
		t.Fatal(p.Err)
	}
	p := s.Run("begin; insert into paged values (1); select generate_series(1, 1000000)")
	if p.Done || !p.InTransaction || !s.InTransaction() {
		t.Fatalf("page = done %v, in transaction %v, %v", p.Done, p.InTransaction, s.InTransaction())
	}
}

// Once set, the search_path is read again after each run, as a rollback can
// undo it.
func TestSearchPathFollowsARollback(t *testing.T) {
	s := newTestSession(t)
	if p := s.Run("begin; set search_path = pg_catalog"); p.Err != nil || s.SearchPath() != "pg_catalog" {
		t.Fatalf("set = %q, %v", s.SearchPath(), p.Err)
	}
	if p := s.Run("rollback"); p.Err != nil || s.SearchPath() == "pg_catalog" {
		t.Fatalf("after rollback = %q, %v", s.SearchPath(), p.Err)
	}
}
