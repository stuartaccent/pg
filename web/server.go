// Package web is the UI: plain HTTP handlers returning templ fragments for
// HTMX. The same handler is served inside the Wails window and, for
// development, in a normal browser via cmd/dev.
package web

import (
	"context"
	"embed"
	"errors"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/catalog"
	"pg/internal/conn"
	"pg/internal/dockerpg"
	"pg/internal/history"
	"pg/internal/session"
)

//go:embed static
var static embed.FS

// Server holds one query session per tab and the tree's catalog, all for the
// connected service, plus the backup or sandbox job in progress.
type Server struct {
	// ChooseFile asks where to save a file, offering name in dir, or wherever
	// was last used if dir is "". It returns "" if cancelled, and is set before
	// serving. Without it, as in a browser, exports go to ~/Downloads and
	// queries are named in a dialog of the page's own.
	ChooseFile func(dir, name string) (string, error)
	// OpenFile asks which .sql file to open, starting in dir, and returns "" if
	// cancelled. Without it, as in a browser, only saved queries can be opened.
	OpenFile func(dir string) (string, error)

	mu       sync.Mutex
	service  string                                        // guarded by mu
	base     string                                        // guarded by mu; service without any /database
	readOnly bool                                          // guarded by mu
	dsn      string                                        // guarded by mu
	settings conn.Settings                                 // guarded by mu
	image    string                                        // guarded by mu; the matching Postgres image
	connect  func(context.Context) (*pgconn.PgConn, error) // guarded by mu; nil until connected
	sessions map[string]*session.Session                   // guarded by mu; by tab id
	cat      *catalog.Catalog                              // guarded by mu
	lastTab  int                                           // guarded by mu
	job      *job                                          // guarded by mu; the latest one
	exportTo map[string]string                             // guarded by mu; where a tab's export was told to go
	hist     *history.Store                                // guarded by mu; nil until first used
}

func New() *Server {
	return &Server{sessions: map[string]*session.Session{}, exportTo: map[string]string{}}
}

// Handler returns the app's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(static))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /connect", s.connectService)
	mux.HandleFunc("GET /services", s.services)
	mux.HandleFunc("POST /config/{file}", s.editConfig)
	mux.HandleFunc("POST /psql", s.openPsql)
	mux.HandleFunc("GET /backups", s.backups)
	mux.HandleFunc("POST /backups/open", s.openBackups)
	mux.HandleFunc("POST /backup", s.backup)
	mux.HandleFunc("POST /backups/verify", s.verifyBackup)
	mux.HandleFunc("POST /backups/restore", s.restoreBackup)
	mux.HandleFunc("POST /backups/replace", s.replaceBackup)
	mux.HandleFunc("POST /backups/delete", s.deleteBackup)
	mux.HandleFunc("POST /sandboxes", s.newSandbox)
	mux.HandleFunc("POST /sandboxes/{name}/remove", s.removeSandbox)
	mux.HandleFunc("GET /job", s.jobStatus)
	mux.HandleFunc("POST /job/cancel", s.cancelJob)
	mux.HandleFunc("POST /tabs", s.newTab)
	mux.HandleFunc("DELETE /tabs/{id}", s.closeTab)
	mux.HandleFunc("POST /query", s.query)
	mux.HandleFunc("GET /query/{id}/more", s.more)
	mux.HandleFunc("POST /cancel", s.cancel)
	mux.HandleFunc("POST /check", s.check)
	mux.HandleFunc("GET /history", s.showHistory)
	mux.HandleFunc("GET /history/list", s.listHistory)
	mux.HandleFunc("GET /history/{id}", s.historyDetail)
	mux.HandleFunc("POST /history/{id}/star", s.starHistory)
	mux.HandleFunc("POST /history/{id}/note", s.noteHistory)
	mux.HandleFunc("DELETE /history/{id}", s.removeHistory)
	mux.HandleFunc("POST /history/clear", s.clearHistory)
	mux.HandleFunc("POST /export/choose", s.chooseExport)
	mux.HandleFunc("POST /export", s.export)
	mux.HandleFunc("GET /tree", s.tree)
	mux.HandleFunc("GET /definition", s.definition)
	mux.HandleFunc("GET /completions", s.completions)
	mux.HandleFunc("GET /table", s.table)
	mux.HandleFunc("GET /queries", s.queries)
	mux.HandleFunc("POST /queries", s.saveQuery)
	mux.HandleFunc("POST /queries/choose", s.chooseQuery)
	mux.HandleFunc("POST /queries/open", s.openQueries)
	mux.HandleFunc("POST /queries/pick", s.pickQuery)
	mux.HandleFunc("GET /queries/{name...}", s.readQuery)
	mux.HandleFunc("GET /activity", s.activity)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /activity/{pid}/{action}", s.stopBackend)
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

// jobCleanupTimeout is how long quitting waits for a job to tidy up.
const jobCleanupTimeout = 20 * time.Second

// Close stops any job and closes the connections, for when the app quits. It
// waits for the job, which has things to remove: exiting under it would leave a
// partial backup or a half-restored database behind, out of sight.
func (s *Server) Close() {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if j != nil {
		j.cancel()
		select {
		case <-j.finished:
		case <-time.After(jobCleanupTimeout):
		}
	}
	s.disconnect()
	s.mu.Lock()
	if s.hist != nil {
		_ = s.hist.Close()
		s.hist = nil
	}
	s.mu.Unlock()
}

// disconnect closes every session and the catalog, and forgets the service.
func (s *Server) disconnect() {
	s.mu.Lock()
	sessions, cat := s.sessions, s.cat
	s.sessions, s.cat, s.connect, s.service, s.readOnly = map[string]*session.Session{}, nil, nil, "", false
	s.dsn, s.settings, s.image, s.base = "", conn.Settings{}, "", ""
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
	services, _, err := serviceList(r.Context())

	// A page load starts over with one tab, so the old tabs' sessions go.
	s.mu.Lock()
	old := s.sessions
	s.sessions = map[string]*session.Session{}
	s.lastTab = 1
	current, readOnly, j := s.service, s.readOnly, s.job
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	closeSessions(ctx, old)

	render(w, r, Index(services, current, readOnly, j.view(), err))
}

func (s *Server) connectService(w http.ResponseWriter, r *http.Request) {
	service := r.FormValue("service")
	services, sandboxes, err := serviceList(r.Context())
	// service/database reaches another database on a service's server, for
	// connecting to one that's just been restored. A service can have a slash in
	// its own name, so that comes first; a database name never has one.
	base, database := service, ""
	if i := strings.LastIndex(service, "/"); i >= 0 && !slices.Contains(services, service) {
		base, database = service[:i], service[i+1:]
	}
	if err == nil && !slices.Contains(services, base) {
		err = errors.New("unknown service " + strconv.Quote(base))
	}
	if err != nil {
		s.connectFailed(w, r, services, err)
		return
	}
	dsn := conn.ServiceDSN(base)
	if i := slices.IndexFunc(sandboxes, func(sb dockerpg.Sandbox) bool { return sb.Name == base }); i >= 0 {
		dsn = sandboxes[i].DSN()
	}
	if database != "" {
		dsn = conn.WithDatabase(dsn, database)
	}

	// Sessions and the catalog connect when first used. This one proves the
	// service works now, and reports what it is.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, err := conn.Open(ctx, dsn)
	if err != nil {
		s.connectFailed(w, r, services, err)
		return
	}
	readOnly := conn.ReadOnly(c)
	image := dockerpg.Image(c.ParameterStatus("server_version"))
	_ = c.Close(ctx)
	settings, err := conn.SettingsFor(dsn)
	if err != nil {
		s.connectFailed(w, r, services, err)
		return
	}

	connect := func(ctx context.Context) (*pgconn.PgConn, error) {
		return conn.Open(ctx, dsn)
	}
	s.disconnect()
	s.mu.Lock()
	s.service, s.base, s.readOnly, s.connect, s.dsn, s.settings, s.image = service, base, readOnly, connect, dsn, settings, image
	s.cat = catalog.New(connect)
	s.mu.Unlock()
	w.Header().Set("HX-Trigger", "connected, services-changed") // reloads the tree and the dropdown
	render(w, r, ConnectStatus(service, readOnly, nil))
}

// serviceList returns what can be connected to: the services, and any
// sandboxes after them. Having no Docker just means having no sandboxes.
func serviceList(ctx context.Context) ([]string, []dockerpg.Sandbox, error) {
	services, err := conn.Services()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sandboxes, _ := dockerpg.Sandboxes(ctx)
	for _, sb := range sandboxes {
		services = append(services, sb.Name)
	}
	return services, sandboxes, err
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
	services, _, err := serviceList(r.Context())
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

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
