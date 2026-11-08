package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pg/internal/conn"
	"pg/internal/dockerpg"
)

// job is a slow piece of work running in the background.
type job struct {
	label    string
	cancel   context.CancelFunc
	finished chan struct{} // closed once it has cleaned up after itself

	mu     sync.Mutex
	line   string // the latest progress
	done   bool
	result jobResult
	err    error
	told   bool // whether the page has seen it finish
}

// jobResult is what a finished job has to say.
type jobResult struct {
	Text    string
	Connect string // a service to offer connecting to, if any
	Reload  bool   // whether what the page shows of the database is out of date
}

// jobView is a job as of now, for rendering.
type jobView struct {
	Label, Line string
	Result      jobResult
	Running     bool
	Err         error
}

// view works on a nil job, which is the usual state of things.
func (j *job) view() jobView {
	if j == nil {
		return jobView{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return jobView{Label: j.label, Line: j.line, Result: j.result, Running: !j.done, Err: j.err}
}

// RunningJob returns the label of the job in progress, or "" if there isn't one.
func (s *Server) RunningJob() string {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if v := j.view(); v.Running {
		return v.Label
	}
	return ""
}

func (j *job) progress(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.line = line
}

// start runs work in the background, one job at a time. It deliberately isn't
// tied to the request, or to the service, as a backup outlasts both.
func (s *Server) start(label string, work func(context.Context, func(string)) (jobResult, error)) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.view().Running {
		return nil, errors.New("wait for " + s.job.label + " to finish")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{label: label, cancel: cancel, finished: make(chan struct{})}
	s.job = j
	go func() {
		defer cancel()
		result, err := work(ctx, j.progress)
		// Only if it failed: Stop can arrive too late, when the work has already
		// finished, and that has to be reported as done.
		if err != nil && ctx.Err() != nil {
			err = errors.New("stopped") // the tools' own exit codes mean nothing here
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		j.done, j.result, j.err = true, result, err
		close(j.finished)
	}()
	return j, nil
}

func (s *Server) jobStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	v := j.view()
	if j != nil && !v.Running {
		j.mu.Lock()
		if !j.told {
			j.told = true
			trigger := "services-changed" // a sandbox may have appeared
			if v.Result.Reload {
				trigger += ", connected" // reloads the tree
			}
			w.Header().Set("HX-Trigger", trigger)
		}
		j.mu.Unlock()
	}
	render(w, r, JobStatus(v))
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if j != nil {
		j.cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

// backupDir returns where backups live, private to the user as they hold
// everything in the database.
func backupDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "pg-backups")
	return dir, os.MkdirAll(dir, 0o700)
}

type backupFile struct {
	Name, Size string
	modified   time.Time
}

// listBackups returns the backups in dir, newest first.
func listBackups(dir string) ([]backupFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []backupFile
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || filepath.Ext(e.Name()) != ".backup" {
			continue
		}
		files = append(files, backupFile{Name: e.Name(), Size: humanSize(info.Size()), modified: info.ModTime()})
	}
	slices.SortFunc(files, func(a, b backupFile) int { return cmp.Compare(b.modified.UnixNano(), a.modified.UnixNano()) })
	return files, nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	size, i := float64(n)/unit, 0
	for size >= unit && i < 3 {
		size /= unit
		i++
	}
	return fmt.Sprintf("%.1f %cB", size, "KMGT"[i])
}

func (s *Server) renderBackups(w http.ResponseWriter, r *http.Request, err error) {
	s.mu.Lock()
	service, canRestore, database := s.service, s.service != "" && !s.readOnly, s.settings.Database
	s.mu.Unlock()
	if dockerpg.CanReplace(database) != "" {
		database = "" // nothing to offer replacing
	}
	dir, derr := backupDir()
	var files []backupFile
	if derr == nil {
		files, derr = listBackups(dir)
	}
	_, sandboxes, _ := serviceList(r.Context())
	render(w, r, BackupsBody(service, database, canRestore, dir, files, sandboxes, cmp.Or(err, derr)))
}

func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	s.renderBackups(w, r, nil)
}

// openFolder shows path in the Finder. It's a variable for the tests.
var openFolder = func(path string) error {
	return exec.Command("open", path).Run()
}

func (s *Server) openBackups(w http.ResponseWriter, r *http.Request) {
	dir, err := backupDir()
	if err == nil {
		err = openFolder(dir)
	}
	if err != nil {
		s.renderBackups(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	service, settings, image := s.service, s.settings, s.image
	s.mu.Unlock()
	if service == "" {
		render(w, r, JobStatus(jobView{Label: "Backup", Err: errors.New("connect to a service first")}))
		return
	}
	schema := r.FormValue("schema") != ""
	label := "Backing up " + service
	if schema {
		label = "Backing up the schema of " + service
	}
	s.startAndRender(w, r, label, func(ctx context.Context, progress func(string)) (jobResult, error) {
		saved, err := saveBackup(ctx, settings, image, schema, progress)
		return jobResult{Text: saved}, err
	})
}

// schemaSuffix ends the name of a backup without rows, so the Backups dialog
// can leave out Replace. Replace checks the file itself as well.
const schemaSuffix = "-schema.backup"

func saveBackup(ctx context.Context, settings conn.Settings, image string, schema bool, progress func(string)) (string, error) {
	dir, err := backupDir()
	if err != nil {
		return "", err
	}
	name := dockerpg.BackupName(settings.Database, image, time.Now())
	if schema {
		name = strings.TrimSuffix(name, ".backup") + schemaSuffix
	}
	dest := filepath.Join(dir, name)
	if err := dockerpg.Backup(ctx, settings, image, dest, schema, progress); err != nil {
		return "", err
	}
	return "Saved " + filepath.Base(dest), nil
}

// backupPath returns the path of a backup named by the page, which has to be
// one in the backup folder.
func backupPath(name string) (string, error) {
	dir, err := backupDir()
	if err != nil {
		return "", err
	}
	if name != filepath.Base(name) || !strings.HasSuffix(name, ".backup") {
		return "", errors.New("not a backup: " + name)
	}
	path := filepath.Join(dir, name)
	_, err = os.Stat(path)
	return path, err
}

// deleteBackup removes a backup for good, rather than to the Trash, as these can
// hold sensitive data. The page asks first.
func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	path, err := backupPath(r.FormValue("file"))
	if err == nil {
		err = os.Remove(path)
	}
	s.renderBackups(w, r, err)
}

func (s *Server) newSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("file")
	path, err := backupPath(name)
	if err != nil {
		render(w, r, JobStatus(jobView{Label: "Sandbox", Err: err}))
		return
	}
	s.startAndRender(w, r, "Opening sandbox", func(ctx context.Context, progress func(string)) (jobResult, error) {
		sb, skipped, err := dockerpg.NewSandbox(ctx, dockerpg.ImageForBackup(name), path, progress)
		if err != nil {
			return jobResult{}, err
		}
		text := sb.Name + " is ready."
		if skipped.Count > 0 {
			text += fmt.Sprintf(" The restore skipped %d errors.", skipped.Count)
		}
		return jobResult{Text: text, Connect: sb.Name}, nil
	})
}

func (s *Server) verifyBackup(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("file")
	path, err := backupPath(name)
	if err != nil {
		render(w, r, JobStatus(jobView{Label: "Verify", Err: err}))
		return
	}
	s.mu.Lock()
	connect, database := s.connect, s.settings.Database
	s.mu.Unlock()
	if !dockerpg.SameDatabase(name, database) {
		connect = nil
	}
	s.startAndRender(w, r, "Verifying "+name, func(ctx context.Context, progress func(string)) (jobResult, error) {
		report, err := dockerpg.Verify(ctx, dockerpg.ImageForBackup(name), path, progress)
		if err != nil {
			return jobResult{}, err
		}
		if problem := report.Problem(); problem != "" {
			return jobResult{}, errors.New(problem)
		}
		if report.SchemaOnly {
			return jobResult{Text: fmt.Sprintf("Verified %s: schema only, %d tables.", name, len(report.Counts))}, nil
		}
		summary := fmt.Sprintf("Verified %s: %d tables, %d rows.", name, len(report.Counts), report.Rows())
		if connect == nil {
			return jobResult{Text: summary}, nil
		}
		progress("Comparing with the live database")
		live, err := liveCounts(ctx, connect)
		if err != nil {
			return jobResult{Text: summary + " Couldn't compare with the live database: " + err.Error()}, nil
		}
		return jobResult{Text: summary + " " + describeDifferences(dockerpg.Differences(report.Counts, live))}, nil
	})
}

// writableTarget returns a backup's path, and the connected server to write it
// to, if that's allowed.
func (s *Server) writableTarget(name string) (string, string, dockerpg.Target, error) {
	path, err := backupPath(name)
	s.mu.Lock()
	service, readOnly := s.service, s.readOnly
	target := dockerpg.Target{DSN: s.dsn, Settings: s.settings, Image: s.image}
	s.mu.Unlock()
	switch {
	case err != nil:
	case service == "":
		err = errors.New("connect to a service first")
	case readOnly:
		err = errors.New(service + " is read-only")
	}
	return path, service, target, err
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	database := r.FormValue("database")
	path, service, target, err := s.writableTarget(r.FormValue("file"))
	if err == nil && !dockerpg.ValidDatabase(database) {
		err = errors.New("not a usable database name: " + database)
	}
	if err != nil {
		render(w, r, JobStatus(jobView{Label: "Restore", Err: err}))
		return
	}
	// The part before any slash, so connecting works from a restored database too.
	base, _, _ := strings.Cut(service, "/")
	s.startAndRender(w, r, "Restoring to "+database, func(ctx context.Context, progress func(string)) (jobResult, error) {
		report, err := dockerpg.Restore(ctx, target, path, database, progress)
		if err != nil {
			return jobResult{}, err
		}
		text := fmt.Sprintf("Restored to %s: %d tables, %d rows.", database, len(report.Counts), report.Rows())
		if report.Ignored > 0 {
			text += fmt.Sprintf(" It skipped %d errors, so check it before relying on it.", report.Ignored)
		}
		return jobResult{Text: text, Connect: base + "/" + database}, nil
	})
}

func (s *Server) replaceBackup(w http.ResponseWriter, r *http.Request) {
	path, _, target, err := s.writableTarget(r.FormValue("file"))
	database := target.Settings.Database
	switch {
	case err != nil:
	case dockerpg.CanReplace(database) != "":
		err = errors.New(dockerpg.CanReplace(database))
	case r.FormValue("confirm") != database:
		err = errors.New("the name typed doesn't match " + database)
	}
	if err != nil {
		render(w, r, JobStatus(jobView{Label: "Replace", Err: err}))
		return
	}
	s.startAndRender(w, r, "Replacing "+database, func(ctx context.Context, progress func(string)) (jobResult, error) {
		report, old, err := dockerpg.Replace(ctx, target, path, database, time.Now(), progress)
		if err != nil {
			return jobResult{}, err
		}
		return jobResult{
			Text:   fmt.Sprintf("Replaced %s: %d tables, %d rows. The previous one is now %s, until you drop it.", database, len(report.Counts), report.Rows(), old),
			Reload: true,
		}, nil
	})
}

// liveCounts counts a live database's rows, in a transaction that can't write.
func liveCounts(ctx context.Context, connect func(context.Context) (*pgconn.PgConn, error)) (map[string]int64, error) {
	c, err := connect(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	if _, err := c.Exec(ctx, "begin read only").ReadAll(); err != nil {
		return nil, err
	}
	return dockerpg.TableCounts(ctx, c)
}

// describeDifferences sums them up. They aren't a fault: a live database is
// meant to move on from its backups.
func describeDifferences(found []string) string {
	const shown = 5
	switch {
	case len(found) == 0:
		return "Matches the live database exactly."
	case len(found) > shown:
		return fmt.Sprintf("%d tables differ from the live database, such as %s.", len(found), strings.Join(found[:shown], "; "))
	}
	return fmt.Sprintf("%d differ from the live database: %s.", len(found), strings.Join(found, "; "))
}

func (s *Server) startAndRender(w http.ResponseWriter, r *http.Request, label string, work func(context.Context, func(string)) (jobResult, error)) {
	j, err := s.start(label, work)
	if err != nil {
		render(w, r, JobStatus(jobView{Label: label, Err: err}))
		return
	}
	render(w, r, JobStatus(j.view()))
}

func (s *Server) removeSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, sandboxes, _ := serviceList(r.Context())
	if !slices.ContainsFunc(sandboxes, func(sb dockerpg.Sandbox) bool { return sb.Name == name }) {
		http.NotFound(w, r) // only ever a sandbox, whatever the URL says
		return
	}
	s.mu.Lock()
	// Any database inside it counts too.
	base, _, _ := strings.Cut(s.service, "/")
	connected := base == name
	s.mu.Unlock()
	if connected {
		s.disconnect()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	err := dockerpg.Remove(ctx, name)
	if connected {
		w.Header().Set("HX-Refresh", "true") // the page was showing what just went
	} else {
		w.Header().Set("HX-Trigger", "services-changed")
	}
	s.renderBackups(w, r, err)
}
