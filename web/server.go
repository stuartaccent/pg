// Package web is the UI: plain HTTP handlers returning templ fragments for
// HTMX. The same handler is served inside the Wails window and, for
// development, in a normal browser via cmd/dev.
package web

import (
	"context"
	"embed"
	"errors"
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

// Server holds the app's single query session and the tree's catalog.
type Server struct {
	mu      sync.Mutex
	service string           // guarded by mu
	sess    *session.Session // guarded by mu
	cat     *catalog.Catalog // guarded by mu
}

func New() *Server { return &Server{} }

// Handler returns the app's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(static))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /connect", s.connect)
	mux.HandleFunc("POST /query", s.query)
	mux.HandleFunc("GET /query/{id}/more", s.more)
	mux.HandleFunc("POST /cancel", s.cancel)
	mux.HandleFunc("GET /tree", s.tree)
	return mux
}

// Close closes the current session and catalog, if any.
func (s *Server) Close() {
	s.mu.Lock()
	sess, cat := s.sess, s.cat
	s.sess, s.cat, s.service = nil, nil, ""
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if sess != nil {
		_ = sess.Close(ctx)
	}
	if cat != nil {
		_ = cat.Close(ctx)
	}
}

func (s *Server) current() (*session.Session, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess, s.service
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	services, err := conn.Services()
	_, current := s.current()
	render(w, r, Index(services, current, err))
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	service := r.FormValue("service")
	services, err := conn.Services()
	if err == nil && !slices.Contains(services, service) {
		err = errors.New("unknown service " + strconv.Quote(service))
	}
	if err != nil {
		render(w, r, ConnectStatus("", err))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, err := conn.Connect(ctx, service)
	if err != nil {
		render(w, r, ConnectStatus("", err))
		return
	}

	connect := func(ctx context.Context) (*pgconn.PgConn, error) {
		return conn.Connect(ctx, service)
	}
	s.Close()
	s.mu.Lock()
	s.service = service
	s.sess = session.New(c, connect)
	s.cat = catalog.New(connect)
	s.mu.Unlock()
	w.Header().Set("HX-Trigger", "connected") // reloads the tree
	render(w, r, ConnectStatus(service, nil))
}

func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.current()
	if sess == nil {
		render(w, r, Results(session.Page{Done: true, Err: errors.New("connect to a service first")}))
		return
	}
	render(w, r, Results(sess.Run(r.FormValue("sql"))))
}

func (s *Server) more(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad query id", http.StatusBadRequest)
		return
	}
	sess, _ := s.current()
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
	render(w, r, MoreRows(p))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if sess, _ := s.current(); sess != nil {
		sess.Cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cat := s.cat
	s.mu.Unlock()
	if cat == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	nodes, err := cat.Children(ctx, r.FormValue("kind"), r.FormValue("id"))
	render(w, r, TreeNodes(nodes, err))
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
