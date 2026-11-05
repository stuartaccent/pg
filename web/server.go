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
	mux.HandleFunc("POST /tabs", s.newTab)
	mux.HandleFunc("DELETE /tabs/{id}", s.closeTab)
	mux.HandleFunc("POST /query", s.query)
	mux.HandleFunc("GET /query/{id}/more", s.more)
	mux.HandleFunc("POST /cancel", s.cancel)
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
		render(w, r, ConnectStatus("", false, err))
		return
	}

	// Sessions and the catalog connect when first used. This one proves the
	// service works now, and reports whether it's set up read-only.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, err := conn.Connect(ctx, service)
	if err != nil {
		render(w, r, ConnectStatus("", false, err))
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
		render(w, r, Results(sess.Explain(r.FormValue("sql")), tab))
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
	schemas := map[string]map[string][]string{}
	if cat := s.catalog(); cat != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var err error
		if schemas, err = cat.Completions(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(schemas)
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
