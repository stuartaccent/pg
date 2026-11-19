// The server tools in the toolbar: Activity, Health and psql.

package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"pg/internal/catalog"
	"pg/internal/conn"
	"pg/internal/dockerpg"
	"pg/internal/term"
)

// psqlTerm is psql running on a pseudo-terminal, for the psql tab.
type psqlTerm struct {
	pty    term.Term
	out    chan []byte        // what psql writes; closed when it exits
	done   chan struct{}      // closed when psql, or docker, has exited
	cancel context.CancelFunc // stops psql, and any container with it
	notice string             // shown before psql's output, if not empty
}

// startPsql starts psql on a pseudo-terminal, with a notice of why it's in
// Docker if that's not the user's choice. It's a variable for the tests, which
// have no docker.
var startPsql = func(ctx context.Context, s conn.Settings, readOnly bool, image, dir string) (term.Term, string, error) {
	cmd, notice := dockerpg.Psql(ctx, s, readOnly, image, dir)
	t, err := term.Start(ctx, cmd)
	return t, notice, err
}

// openPsql starts psql on the current connection, database and all, unless
// it's already running. It's the computer's own psql if that's new enough for the
// server, or else the server's own Postgres image.
func (s *Server) openPsql(w http.ResponseWriter, r *http.Request) {
	dir, err := queryDir()
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.term; t != nil {
		select {
		case <-t.done:
			t.cancel() // lets go of output nobody read
		default:
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if s.dsn == "" {
		err = errors.New("connect to a service first")
	}
	if err == nil {
		ctx, cancel := context.WithCancel(context.Background())
		var pty term.Term
		var notice string
		if pty, notice, err = startPsql(ctx, s.settings, s.readOnly, s.image, dir); err != nil {
			cancel()
		} else {
			s.term = &psqlTerm{pty: pty, out: make(chan []byte), done: make(chan struct{}), cancel: cancel, notice: notice}
			go s.term.pump(ctx)
			go func(t *psqlTerm) { _ = t.pty.Wait(); close(t.done) }(s.term)
		}
	}
	if err != nil {
		render(w, r, ConnectStatus("", false, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pump passes psql's output on to whoever is showing it, until psql exits or
// is stopped.
func (t *psqlTerm) pump(ctx context.Context) {
	defer close(t.out)
	defer t.pty.Close()
	if t.notice != "" {
		select {
		case t.out <- []byte("\x1b[33m" + t.notice + "\x1b[0m\r\n\r\n"): // yellow, then psql's own output
		case <-ctx.Done():
			return
		}
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			select {
			case t.out <- bytes.Clone(buf[:n]):
			case <-ctx.Done():
				return
			}
		}
		if err != nil { // EIO once psql has exited, or EOF on Windows
			return
		}
	}
}

// psqlOutput returns what psql has written since the last call, waiting for
// some if there's none yet, or no content once psql has exited. Each call is
// a whole response rather than one long stream, as WebKit can hold back the
// tail of a stream until more arrives, which left psql's prompt undrawn until
// the next key.
func (s *Server) psqlOutput(w http.ResponseWriter, r *http.Request) {
	t := s.psql()
	if t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var out []byte
	select {
	case b, ok := <-t.out:
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		out = b
	case <-r.Context().Done():
		return
	}
	// Whatever else is ready goes too, so a burst takes one round trip. Once
	// psql has exited, the next call says so.
	for more := true; more; {
		select {
		case b, ok := <-t.out:
			out, more = append(out, b...), ok
		default:
			more = false
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(out)
}

// psqlInput types what the tab sends into psql.
func (s *Server) psqlInput(w http.ResponseWriter, r *http.Request) {
	if t := s.psql(); t != nil {
		_, _ = io.Copy(t.pty, http.MaxBytesReader(w, r.Body, 1<<20)) // fails once psql has gone, which the tab sees anyway
	}
	w.WriteHeader(http.StatusNoContent)
}

// psqlSize tells psql the tab's size, in characters, so its output wraps there.
func (s *Server) psqlSize(w http.ResponseWriter, r *http.Request) {
	cols, cerr := strconv.ParseUint(r.FormValue("cols"), 10, 16)
	rows, rerr := strconv.ParseUint(r.FormValue("rows"), 10, 16)
	if cerr != nil || rerr != nil {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	if t := s.psql(); t != nil {
		_ = t.pty.Resize(uint16(cols), uint16(rows))
	}
	w.WriteHeader(http.StatusNoContent)
}

// closePsql stops psql, as its tab has closed.
func (s *Server) closePsql(w http.ResponseWriter, r *http.Request) {
	s.endPsql()
	w.WriteHeader(http.StatusNoContent)
}

// endPsql stops psql and returns once it has exited, or nil if there was none.
func (s *Server) endPsql() <-chan struct{} {
	s.mu.Lock()
	t := s.term
	s.term = nil
	s.mu.Unlock()
	if t == nil {
		return nil
	}
	t.cancel()
	return t.done
}

func (s *Server) psql() *psqlTerm {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term
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
