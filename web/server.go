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
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/jackc/pgx/v5/pgconn"
	pg_query "github.com/pganalyze/pg_query_go/v6"

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
	s.dsn, s.settings, s.image = "", conn.Settings{}, ""
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
	// its own name, so that comes first.
	base, database := service, ""
	if !slices.Contains(services, service) {
		base, database, _ = strings.Cut(service, "/")
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
	s.service, s.readOnly, s.connect, s.dsn, s.settings, s.image = service, readOnly, connect, dsn, settings, image
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

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	s.renderActivity(w, r, nil)
}

// renderActivity lists the server's connections, after any stopErr.
func (s *Server) renderActivity(w http.ResponseWriter, r *http.Request, stopErr error) {
	s.mu.Lock()
	cat, readOnly := s.cat, s.readOnly
	s.mu.Unlock()
	if cat == nil {
		render(w, r, ActivityBody(nil, false, errors.New("connect to a service first")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	backends, err := cat.Activity(ctx)
	render(w, r, ActivityBody(backends, !readOnly, errors.Join(stopErr, err)))
}

// health runs the health checks on the connected database.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cat := s.cat
	s.mu.Unlock()
	if cat == nil {
		render(w, r, HealthBody(catalog.Report{}, errors.New("connect to a service first")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	report, err := cat.Health(ctx)
	render(w, r, HealthBody(report, err))
}

// stopBackend cancels a connection's query or ends the connection. Neither
// writes anything, but read-only is there to keep a live service safe from
// slips, and this could stop someone's work.
func (s *Server) stopBackend(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(r.PathValue("pid"))
	action := r.PathValue("action")
	if err != nil || (action != "cancel" && action != "terminate") {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	cat, readOnly := s.cat, s.readOnly
	s.mu.Unlock()
	switch {
	case cat == nil:
		err = errors.New("connect to a service first")
	case readOnly:
		err = errors.New("this service is read-only")
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		err = cat.Stop(ctx, pid, action == "terminate")
	}
	s.renderActivity(w, r, err)
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.Render(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
