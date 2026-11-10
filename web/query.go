// The query tabs: running, paging, cancelling, checking and exporting.

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"pg/internal/conn"
	"pg/internal/session"

	"github.com/jackc/pgx/v5/pgconn"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func (s *Server) newTab(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.lastTab++
	id := strconv.Itoa(s.lastTab)
	s.mu.Unlock()
	render(w, r, NewTab(id))
}

func (s *Server) closeTab(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if sess != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_ = sess.Close(ctx)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	tab := r.FormValue("tab")
	sess := s.session(tab)
	if sess == nil {
		render(w, r, Results(session.Page{Done: true, Err: errors.New("connect to a service first")}, tab))
		return
	}
	// As typed, so running it again from History asks for its params again.
	if typed := r.FormValue("typed"); typed != "" {
		s.remember(r.Context(), typed, r.FormValue("sql"))
	}
	if r.FormValue("explain") != "" {
		render(w, r, Results(sess.Explain(r.FormValue("sql"), r.FormValue("explain") == "analyze"), tab))
		return
	}
	render(w, r, Results(sess.Run(r.FormValue("sql")), tab))
}

func (s *Server) more(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad query id", http.StatusBadRequest)
		return
	}
	tab := r.FormValue("tab")
	sess := s.session(tab)
	if sess == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, err := sess.More(id)
	if err != nil {
		// A newer query replaced this one; its sentinel row just goes away.
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.FormValue("next") != "" {
		render(w, r, Results(p, tab))
		return
	}
	render(w, r, MoreRows(p, tab))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if sess := s.session(r.FormValue("tab")); sess != nil {
		sess.Cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

// check is what the page asks about sql before running it: whether it writes
// to every row. SQL that doesn't parse doesn't.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"everyRow": writesEveryRow(r.FormValue("sql"))})
}

// writesEveryRow reports whether sql has an update or delete with no where,
// anywhere, including inside a with.
func writesEveryRow(sql string) bool {
	tree, err := pg_query.ParseToJSON(sql)
	if err != nil {
		return false
	}
	var node any
	if err := json.Unmarshal([]byte(tree), &node); err != nil {
		return false
	}
	return anyWithoutWhere(node)
}

// anyWithoutWhere walks a parse tree, as JSON, for an UpdateStmt or DeleteStmt
// with no whereClause. Walking the JSON saves a case for every node type.
func anyWithoutWhere(node any) bool {
	switch n := node.(type) {
	case map[string]any:
		for key, child := range n {
			if stmt, ok := child.(map[string]any); ok && (key == "UpdateStmt" || key == "DeleteStmt") && stmt["whereClause"] == nil {
				return true
			}
			if anyWithoutWhere(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(n, anyWithoutWhere)
	}
	return false
}

// chooseExport is an export's first step: it asks where to save, then tells the
// page to start. The path stays here, so the page can't name one for /export.
func (s *Server) chooseExport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	connected := s.connect != nil
	s.mu.Unlock()
	if !connected {
		render(w, r, ExportStatus("", 0, errors.New("connect to a service first")))
		return
	}
	if s.ChooseFile != nil {
		path, err := s.ChooseFile("", "export.csv")
		if err != nil {
			render(w, r, ExportStatus("", 0, err))
			return
		}
		if path == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mu.Lock()
		s.exportTo[r.FormValue("tab")] = path
		s.mu.Unlock()
	}
	w.Header().Set("HX-Trigger", "export-ready")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	tab := r.FormValue("tab")
	s.mu.Lock()
	connect, chosen := s.connect, s.exportTo[tab]
	delete(s.exportTo, tab)
	s.mu.Unlock()
	if connect == nil {
		render(w, r, ExportStatus("", 0, errors.New("connect to a service first")))
		return
	}
	if s.ChooseFile != nil && chosen == "" {
		render(w, r, ExportStatus("", 0, errors.New("choose where to save first")))
		return
	}
	path, rows, err := saveCSV(r.Context(), connect, r.FormValue("sql"), chosen)
	render(w, r, ExportStatus(path, rows, err))
}

// saveCSV writes query's rows to path, or a new file in ~/Downloads if that's
// empty, on a connection of its own so the tab's result isn't disturbed. Going
// straight to a file keeps large results out of memory, and webviews needn't
// support downloads. It writes beside path and renames, so a failed export
// leaves a file being replaced as it was.
//
// shortcut: Cancel doesn't reach an export, only leaving the page does. Share
// the session's cancel if long exports turn out to need stopping.
func saveCSV(ctx context.Context, connect func(context.Context) (*pgconn.PgConn, error), query, path string) (string, int64, error) {
	dir, pattern := filepath.Dir(path), ".pg-export-*.csv"
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", 0, err
		}
		dir, pattern = filepath.Join(home, "Downloads"), "pg-export-*.csv"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	c, err := connect(ctx)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(closing)
	}()

	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", 0, err
	}
	rows, err := conn.Export(ctx, c, query, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name()) // half an export is worse than none
		return "", 0, err
	}
	if path == "" {
		return f.Name(), rows, nil
	}
	if err := os.Rename(f.Name(), path); err != nil {
		_ = os.Remove(f.Name())
		return "", 0, err
	}
	return path, rows, nil
}
