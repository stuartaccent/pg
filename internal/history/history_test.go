package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func list(t *testing.T, s *Store, f Filter) []Entry {
	t.Helper()
	entries, err := s.List(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestOneEntryPerQuery(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local)
	for i, typed := range []string{"select * from t where id = :id", "SELECT * FROM t WHERE id = :id"} {
		filled := map[int]string{0: "select * from t where id = '1'", 1: "SELECT * FROM t WHERE id = '2'"}[i]
		if err := s.Record(ctx, typed, filled, "local", "app", start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Record(ctx, "select nope from", "select nope from", "live", "app", start); err != nil {
		t.Fatal(err)
	}

	entries := list(t, s, Filter{})
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	if e.SQL != "SELECT * FROM t WHERE id = :id" || e.Runs != 2 || !e.LastRun.Equal(start.Add(time.Hour)) || e.Services != "local" {
		t.Errorf("newest text, two runs: %+v", e)
	}
	if entries[1].SQL != "select nope from" {
		t.Errorf("SQL that doesn't parse is kept by its text: %+v", entries[1])
	}

	_, runs, err := s.Detail(ctx, e.ID)
	if err != nil || len(runs) != 2 || runs[0].Database != "app" || !runs[0].At.After(runs[1].At) {
		t.Errorf("runs = %+v, %v", runs, err)
	}
	services, err := s.Services(ctx)
	if err != nil || len(services) != 2 || services[0] != "live" {
		t.Errorf("services = %v, %v", services, err)
	}
}

func TestFilters(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	now := time.Now()
	for _, run := range []struct{ sql, service string }{
		{"select * from a", "local"}, {"select * from b", "live"}, {"select * from b", "live"}, {"select '50%' from c", "local"},
	} {
		if err := s.Record(ctx, run.sql, run.sql, run.service, "db", now); err != nil {
			t.Fatal(err)
		}
	}
	if got := list(t, s, Filter{Service: "live"}); len(got) != 1 || got[0].SQL != "select * from b" {
		t.Errorf("by service: %+v", got)
	}
	if got := list(t, s, Filter{Search: "%"}); len(got) != 1 || got[0].SQL != "select '50%' from c" {
		t.Errorf("search for %%: %+v", got)
	}
	if got := list(t, s, Filter{Sort: "runs"}); got[0].SQL != "select * from b" {
		t.Errorf("by runs: %+v", got)
	}

	one := list(t, s, Filter{Search: "from a"})[0]
	if err := s.Star(ctx, one.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNote(ctx, one.ID, "the first"); err != nil {
		t.Fatal(err)
	}
	if got := list(t, s, Filter{Sort: "runs"}); got[0].ID != one.ID {
		t.Errorf("starred first: %+v", got)
	}
	if got := list(t, s, Filter{Starred: true}); len(got) != 1 || got[0].Note != "the first" {
		t.Errorf("starred only: %+v", got)
	}
	if got := list(t, s, Filter{Search: "FIRST"}); len(got) != 1 {
		t.Errorf("search notes: %+v", got)
	}
}

func TestClearAndRemove(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	old, recent := time.Now().AddDate(0, 0, -60), time.Now()
	for _, run := range []struct {
		sql string
		at  time.Time
	}{{"select * from a", old}, {"select * from b", old}, {"select * from b", recent}, {"select * from c", old}} {
		if err := s.Record(ctx, run.sql, run.sql, "local", "db", run.at); err != nil {
			t.Fatal(err)
		}
	}
	three := list(t, s, Filter{Search: "from c"})[0]
	if err := s.Star(ctx, three.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearBefore(ctx, time.Now().AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}
	got := list(t, s, Filter{})
	if len(got) != 2 || got[0].SQL != "select * from c" || !got[0].LastRun.IsZero() || got[0].Runs != 0 || got[1].Runs != 1 {
		t.Fatalf("after clearing: %+v", got)
	}

	if err := s.Remove(ctx, three.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Detail(ctx, three.ID); err != ErrNotFound {
		t.Errorf("removed: err = %v", err)
	}
}

func TestDetailShowsRecentRuns(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local)
	for i := range RecentRuns + 2 {
		if err := s.Record(ctx, "select * from a", "select * from a", "local", "db", start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	e, runs, err := s.Detail(ctx, list(t, s, Filter{})[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Runs != RecentRuns+2 || len(runs) != RecentRuns || !runs[0].At.Equal(start.Add(time.Duration(RecentRuns+1)*time.Minute)) {
		t.Errorf("entry %+v, runs %+v", e, runs)
	}
}
