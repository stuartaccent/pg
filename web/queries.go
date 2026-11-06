package web

import (
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

//go:embed starters
var starters embed.FS

// queryDir returns the folder of saved queries. They're plain files so they
// can be edited anywhere, kept in git, and outlive the app. The starters only
// go in when it's first made, so one that's deleted stays deleted.
func queryDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "pg-queries")
	if err := os.Mkdir(dir, 0o700); errors.Is(err, fs.ErrExist) {
		return dir, nil
	} else if err != nil {
		return "", err
	}
	files, err := fs.Sub(starters, "starters")
	if err != nil {
		return "", err
	}
	return dir, os.CopyFS(dir, files)
}

// listQueries returns the queries in dir and its subfolders, as paths relative
// to it without the .sql. Anything hidden is left out, so that a .git isn't
// searched.
func listQueries(dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case path == dir:
			return nil
		case strings.HasPrefix(d.Name(), ".") && d.IsDir():
			return filepath.SkipDir
		case strings.HasPrefix(d.Name(), "."):
			return nil
		}
		if name, ok := strings.CutSuffix(strings.TrimPrefix(path, dir+"/"), ".sql"); ok && d.Type().IsRegular() {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return names, nil
}

// queryPath returns the path of the query called name, which can be in a
// subfolder but has to stay inside the folder. Ruling out parts that are empty
// or start with a dot rules out .. and absolute paths along with hidden files.
func queryPath(name string) (string, error) {
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return "", errors.New("names and folders can't be empty or start with a dot")
		}
	}
	dir, err := queryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".sql"), nil
}

func (s *Server) renderQueries(w http.ResponseWriter, r *http.Request, err error) {
	dir, derr := queryDir()
	var names []string
	if derr == nil {
		names, derr = listQueries(dir)
	}
	render(w, r, QueriesBody(dir, names, errors.Join(err, derr)))
}

func (s *Server) queries(w http.ResponseWriter, r *http.Request) {
	s.renderQueries(w, r, nil)
}

func (s *Server) openQueries(w http.ResponseWriter, r *http.Request) {
	dir, err := queryDir()
	if err == nil {
		err = openFolder(dir)
	}
	if err != nil {
		s.renderQueries(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readQuery returns a query's text, as definition does an object's.
func (s *Server) readQuery(w http.ResponseWriter, r *http.Request) {
	path, err := queryPath(r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(text)
}

// saveQuery writes a query, stopping to ask if the name is taken.
func (s *Server) saveQuery(w http.ResponseWriter, r *http.Request) {
	text := r.FormValue("text")
	path, err := queryPath(strings.TrimSuffix(strings.TrimSpace(r.FormValue("name")), ".sql"))
	if err == nil && strings.TrimSpace(text) == "" {
		err = errors.New("there's nothing in this tab to save")
	}
	if err == nil {
		err = writeQuery(path, text, r.FormValue("overwrite") != "")
	}
	if err != nil {
		render(w, r, SaveStatus(err))
		return
	}
	w.Header().Set("HX-Trigger", "query-saved")
	w.WriteHeader(http.StatusNoContent)
}

func writeQuery(path, text string, overwrite bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	return errors.Join(err, f.Close())
}
