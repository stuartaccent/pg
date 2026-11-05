// Package web is the UI: plain HTTP handlers returning templ fragments for
// HTMX. The same handler is served inside the Wails window and, for
// development, in a normal browser via cmd/dev.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/catalog"
	"pg/internal/conn"
	"pg/internal/session"
)

//go:embed static
var static embed.FS

// Server holds one query session per tab and the tree's catalog, all for the
// connected service.
type Server struct {
	mu       sync.Mutex
	service  string                                        // guarded by mu
	readOnly bool                                          // guarded by mu
	connect  func(context.Context) (*pgconn.PgConn, error) // guarded by mu; nil until connected
	sessions map[string]*session.Session                   // guarded by mu; by tab id
	cat      *catalog.Catalog                              // guarded by mu
	lastTab  int                                           // guarded by mu
}

func New() *Server { return &Server{sessions: map[string]*session.Session{}} }

// Handler returns the app's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(static))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /connect", s.connectService)
	mux.HandleFunc("GET /services", s.services)
	mux.HandleFunc("POST /config/{file}", s.editConfig)
	mux.HandleFunc("POST /tabs", s.newTab)
	mux.HandleFunc("DELETE /tabs/{id}", s.closeTab)
	mux.HandleFunc("POST /query", s.query)
	mux.HandleFunc("GET /query/{id}/more", s.more)
	mux.HandleFunc("POST /cancel", s.cancel)
	mux.HandleFunc("POST /export", s.export)
	mux.HandleFunc("GET /tree", s.tree)
	mux.HandleFunc("GET /definition", s.definition)
	mux.HandleFunc("GET /completions", s.completions)
	return noStore(mux)
}

// noStore stops the webview reusing responses. Without it WebKit replays GETs,
// so a reloaded tree or a repeated /query/1/more would show stale data.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Close closes every session and the catalog, and forgets the service.
func (s *Server) Close() {
	s.mu.Lock()
	sessions, cat := s.sessions, s.cat
	s.sessions, s.cat, s.connect, s.service, s.readOnly = map[string]*session.Session{}, nil, nil, "", false
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closeSessions(ctx, sessions)
	if cat != nil {
		_ = cat.Close(ctx)
	}
}

func closeSessions(ctx context.Context, sessions map[string]*session.Session) {
	for _, sess := range sessions {
		_ = sess.Close(ctx)
	}
}

// session returns a tab's session, or nil if no service is connected. It
// connects on first use, so a tab costs nothing until it runs something.
func (s *Server) session(tab string) *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connect == nil {
		return nil
	}
	sess := s.sessions[tab]
	if sess == nil {
		sess = session.New(nil, s.connect)
		s.sessions[tab] = sess
	}
	return sess
}

func (s *Server) catalog() *catalog.Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cat
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	services, err := conn.Services()

	// A page load starts over with one tab, so the old tabs' sessions go.
	s.mu.Lock()
	old := s.sessions
	s.sessions = map[string]*session.Session{}
	s.lastTab = 1
	current, readOnly := s.service, s.readOnly
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	closeSessions(ctx, old)

	render(w, r, Index(services, current, readOnly, err))
}

func (s *Server) connectService(w http.ResponseWriter, r *http.Request) {
	service := r.FormValue("service")
	services, err := conn.Services()
	if err == nil && !slices.Contains(services, service) {
		err = errors.New("unknown service " + strconv.Quote(service))
	}
	if err != nil {
		s.connectFailed(w, r, services, err)
		return
	}

	// Sessions and the catalog connect when first used. This one proves the
	// service works now, and reports whether it's set up read-only.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, err := conn.Connect(ctx, service)
	if err != nil {
		s.connectFailed(w, r, services, err)
		return
	}
	readOnly := conn.ReadOnly(c)
	_ = c.Close(ctx)

	connect := func(ctx context.Context) (*pgconn.PgConn, error) {
		return conn.Connect(ctx, service)
	}
	s.Close()
	s.mu.Lock()
	s.service, s.readOnly, s.connect = service, readOnly, connect
	s.cat = catalog.New(connect)
	s.mu.Unlock()
	w.Header().Set("HX-Trigger", "connected") // reloads the tree
	render(w, r, ConnectStatus(service, readOnly, nil))
}

// connectFailed reports err, and puts the dropdown back to what's connected.
// Left on the failed choice, picking it again to retry wouldn't be a change.
func (s *Server) connectFailed(w http.ResponseWriter, r *http.Request, services []string, err error) {
	s.mu.Lock()
	current := s.service
	s.mu.Unlock()
	render(w, r, ConnectFailed(services, current, err))
}

// services lists the service file again, for after it has been edited.
func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	services, err := conn.Services()
	if err != nil {
		w.WriteHeader(http.StatusNoContent) // what's listed already will do
		return
	}
	s.mu.Lock()
	current := s.service
	s.mu.Unlock()
	render(w, r, serviceSelect(services, current))
}

// openInEditor opens path in the user's text editor. It's a variable for the tests.
//
// shortcut: macOS only, which is all pg has run on. Add xdg-open if that changes.
var openInEditor = func(path string) error {
	return exec.Command("open", "-t", path).Run()
}

// editConfig hands a config file to the user's text editor. pg deliberately
// never reads these itself, so passwords don't pass through it.
func (s *Server) editConfig(w http.ResponseWriter, r *http.Request) {
	var path string
	var err error
	switch r.PathValue("file") {
	case "services":
		path, err = conn.ServiceFilePath()
	case "passwords":
		path, err = conn.PassFilePath()
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil {
		err = conn.EnsureFile(path)
	}
	if err == nil {
		err = openInEditor(path)
	}
	if err != nil {
		render(w, r, ConnectStatus("", false, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

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
	switch {
	case sess == nil:
		render(w, r, Results(session.Page{Done: true, Err: errors.New("connect to a service first")}, tab))
	case r.FormValue("explain") != "":
		render(w, r, Results(sess.Explain(r.FormValue("sql"), r.FormValue("explain") == "analyze"), tab))
	default:
		render(w, r, Results(sess.Run(r.FormValue("sql")), tab))
	}
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
	render(w, r, MoreRows(p, tab))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if sess := s.session(r.FormValue("tab")); sess != nil {
		sess.Cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	connect := s.connect
	s.mu.Unlock()
	if connect == nil {
		render(w, r, ExportStatus("", 0, errors.New("connect to a service first")))
		return
	}
	path, rows, err := saveCSV(r.Context(), connect, r.FormValue("sql"))
	render(w, r, ExportStatus(path, rows, err))
}

// saveCSV writes query's rows to a new file in ~/Downloads, on a connection of
// its own so the tab's result isn't disturbed. Going straight to a file keeps
// large results out of memory, and webviews needn't support downloads.
//
// shortcut: Cancel doesn't reach an export, only leaving the page does. Share
// the session's cancel if long exports turn out to need stopping.
func saveCSV(ctx context.Context, connect func(context.Context) (*pgconn.PgConn, error), query string) (string, int64, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Join(home, "Downloads")
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

	f, err := os.CreateTemp(dir, "pg-export-*.csv")
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
	return f.Name(), rows, nil
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	cat := s.catalog()
	if cat == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	nodes, err := cat.Children(ctx, r.FormValue("kind"), r.FormValue("id"))
	render(w, r, TreeNodes(nodes, err))
}

func (s *Server) definition(w http.ResponseWriter, r *http.Request) {
	cat := s.catalog()
	if cat == nil {
		http.Error(w, "connect to a service first", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	def, err := cat.Definition(ctx, r.FormValue("def"), r.FormValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, def)
}

func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	var done catalog.Completions
	if cat := s.catalog(); cat != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var err error
		if done, err = cat.Completions(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(done)
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
