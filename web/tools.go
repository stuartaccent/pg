// The server tools in the toolbar: Activity, Health and psql.

package web

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"pg/internal/catalog"
	"pg/internal/dockerpg"
)

// openTerminal runs an AppleScript, to open Terminal. It's a variable for the tests.
var openTerminal = func(script string) error {
	return exec.Command("osascript", "-e", script).Run()
}

// openPsql opens psql in Terminal on the current connection, database and all.
// It runs in the server's own Postgres image, so its version always matches.
func (s *Server) openPsql(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	connected, settings, image := s.dsn != "", s.settings, s.image
	s.mu.Unlock()
	if !connected {
		render(w, r, ConnectStatus("", false, errors.New("connect to a service first")))
		return
	}
	dir, err := queryDir()
	var cmd string
	if err == nil {
		cmd, err = dockerpg.PsqlCommand(settings, image, dir)
	}
	if err == nil {
		err = openTerminal(terminalScript(cmd))
	}
	if err != nil {
		render(w, r, ConnectStatus("", false, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// terminalScript is the AppleScript that runs a shell command in a new Terminal
// window. Launching Terminal opens a window of its own, so then it uses that
// one rather than leave an empty one beside it.
func terminalScript(cmd string) string {
	cmd = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(cmd) + `"`
	return `set wasRunning to application "Terminal" is running
tell application "Terminal"
	if wasRunning then
		do script ` + cmd + `
	else
		do script ` + cmd + ` in window 1
	end if
	activate
end tell`
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
	cat := s.catalog()
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
