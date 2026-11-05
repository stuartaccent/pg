package session

import (
	"context"
	"errors"
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
	connect := func(ctx context.Context) (*pgconn.PgConn, error) { return conn.Connect(ctx, service) }
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

	p := s.Explain("delete from explained;")
	if p.Err != nil || !p.Done || len(p.Columns) != 1 || len(p.Rows) == 0 {
		t.Fatalf("page = %+v", p)
	}

	// A second statement must be refused, not run.
	if p := s.Explain("select 1; delete from explained"); p.Err == nil {
		t.Fatal("two statements: want an error")
	}
	p = s.Run("select count(*) from explained")
	if p.Err != nil || p.Rows[0][0].Text != "1" {
		t.Fatalf("explain changed the table: %+v", p)
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
	if p := s.Explain("select 1"); !errors.Is(p.Err, ErrClosed) {
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
