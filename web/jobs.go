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

	"pg/internal/conn"
	"pg/internal/dockerpg"
)

// job is a slow piece of work running in the background.
type job struct {
	label  string
	cancel context.CancelFunc

	mu     sync.Mutex
	line   string // the latest progress
	done   bool
	result string
	err    error
	told   bool // whether the page has seen it finish
}

// jobView is a job as of now, for rendering.
type jobView struct {
	Label, Line, Result string
	Running             bool
	Err                 error
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

func (j *job) progress(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.line = line
}

// start runs work in the background, one job at a time. It deliberately isn't
// tied to the request, or to the service, as a backup outlasts both.
func (s *Server) start(label string, work func(context.Context, func(string)) (string, error)) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.view().Running {
		return nil, errors.New("wait for " + s.job.label + " to finish")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{label: label, cancel: cancel}
	s.job = j
	go func() {
		defer cancel()
		result, err := work(ctx, j.progress)
		if ctx.Err() != nil {
			err = errors.New("stopped") // the tools' own exit codes mean nothing here
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		j.done, j.result, j.err = true, result, err
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
			w.Header().Set("HX-Trigger", "services-changed") // a sandbox may have appeared
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
	service := s.service
	s.mu.Unlock()
	dir, derr := backupDir()
	var files []backupFile
	if derr == nil {
		files, derr = listBackups(dir)
	}
	_, sandboxes, _ := serviceList(r.Context())
	render(w, r, BackupsBody(service, dir, files, sandboxes, cmp.Or(err, derr)))
}

func (s *Server) backups(w http.ResponseWriter, r *http.Request) {
	s.renderBackups(w, r, nil)
}

// openFolder shows path in the Finder. It's a variable for the tests.
//
// shortcut: macOS only, like openInEditor.
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
	s.startAndRender(w, r, "Backing up "+service, func(ctx context.Context, progress func(string)) (string, error) {
		return saveBackup(ctx, settings, image, progress)
	})
}

func saveBackup(ctx context.Context, settings conn.Settings, image string, progress func(string)) (string, error) {
	dir, err := backupDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, dockerpg.BackupName(settings.Database, image, time.Now()))
	if err := dockerpg.Backup(ctx, settings, image, dest, progress); err != nil {
		return "", err
	}
	return "Saved " + filepath.Base(dest), nil
}

func (s *Server) newSandbox(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("file")
	dir, err := backupDir()
	if err == nil && (name != filepath.Base(name) || !strings.HasSuffix(name, ".backup")) {
		err = errors.New("not a backup: " + name)
	}
	var path string
	if err == nil {
		path = filepath.Join(dir, name)
		_, err = os.Stat(path)
	}
	if err != nil {
		render(w, r, JobStatus(jobView{Label: "Sandbox", Err: err}))
		return
	}
	s.startAndRender(w, r, "Opening sandbox", func(ctx context.Context, progress func(string)) (string, error) {
		sb, ignored, err := dockerpg.NewSandbox(ctx, dockerpg.ImageForBackup(name), path, progress)
		if err != nil {
			return "", err
		}
		if ignored > 0 {
			return fmt.Sprintf("%s is ready: pick it from the service list. The restore skipped %d errors.", sb.Name, ignored), nil
		}
		return sb.Name + " is ready: pick it from the service list.", nil
	})
}

func (s *Server) startAndRender(w http.ResponseWriter, r *http.Request, label string, work func(context.Context, func(string)) (string, error)) {
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
	connected := s.service == name
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
