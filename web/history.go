package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"pg/internal/history"
)

// history opens the history the first time it's wanted.
func (s *Server) history() (*history.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hist == nil {
		path, err := history.Path()
		if err == nil {
			s.hist, err = history.Open(path)
		}
		if err != nil {
			return nil, err
		}
	}
	return s.hist, nil
}

// remember records a run in the history. History is a convenience, so a
// failure here mustn't stop the query running.
func (s *Server) remember(ctx context.Context, typed, filled string) {
	s.mu.Lock()
	service, database := s.service, s.settings.Database
	s.mu.Unlock()
	if h, err := s.history(); err == nil {
		_ = h.Record(ctx, typed, filled, service, database, time.Now())
	}
}

func historyFilter(r *http.Request) history.Filter {
	return history.Filter{
		Search:  r.FormValue("q"),
		Service: r.FormValue("service"),
		Starred: r.FormValue("starred") != "",
		Sort:    r.FormValue("sort"),
	}
}

// showHistory is the dialog's body: its filters and every entry.
func (s *Server) showHistory(w http.ResponseWriter, r *http.Request) {
	var entries []history.Entry
	var services []string
	h, err := s.history()
	if err == nil {
		services, err = h.Services(r.Context())
	}
	if err == nil {
		entries, err = h.List(r.Context(), history.Filter{})
	}
	render(w, r, HistoryBody(services, entries, err))
}

// listHistory is the entries that match the dialog's filters.
func (s *Server) listHistory(w http.ResponseWriter, r *http.Request) {
	f := historyFilter(r)
	var entries []history.Entry
	h, err := s.history()
	if err == nil {
		entries, err = h.List(r.Context(), f)
	}
	selected, _ := strconv.ParseInt(r.FormValue("selected"), 10, 64)
	render(w, r, HistoryList(entries, f.Sort, selected, err))
}

func (s *Server) historyDetail(w http.ResponseWriter, r *http.Request) {
	var e history.Entry
	var runs []history.Run
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		var h *history.Store
		if h, err = s.history(); err == nil {
			e, runs, err = h.Detail(r.Context(), id)
		}
	}
	render(w, r, HistoryDetail(e, runs, err))
}

// changeHistory runs change on the entry in the path. With refresh it tells the
// dialog to show the change, which a note needn't: it's already on screen.
func (s *Server) changeHistory(w http.ResponseWriter, r *http.Request, refresh bool, change func(context.Context, *history.Store, int64) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad history id", http.StatusBadRequest)
		return
	}
	h, err := s.history()
	if err == nil {
		err = change(r.Context(), h, id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if refresh {
		w.Header().Set("HX-Trigger", "history-changed")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) starHistory(w http.ResponseWriter, r *http.Request) {
	s.changeHistory(w, r, true, func(ctx context.Context, h *history.Store, id int64) error {
		return h.Star(ctx, id, r.FormValue("starred") == "1")
	})
}

func (s *Server) noteHistory(w http.ResponseWriter, r *http.Request) {
	s.changeHistory(w, r, false, func(ctx context.Context, h *history.Store, id int64) error {
		return h.SetNote(ctx, id, r.FormValue("note"))
	})
}

func (s *Server) removeHistory(w http.ResponseWriter, r *http.Request) {
	s.changeHistory(w, r, true, func(ctx context.Context, h *history.Store, id int64) error {
		return h.Remove(ctx, id)
	})
}

// clearHistory clears runs older than days, or all of them. Starred entries
// stay either way.
func (s *Server) clearHistory(w http.ResponseWriter, r *http.Request) {
	before := time.Now()
	if days := r.FormValue("days"); days != "all" {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			http.Error(w, "bad number of days", http.StatusBadRequest)
			return
		}
		before = before.AddDate(0, 0, -n)
	}
	h, err := s.history()
	if err == nil {
		err = h.ClearBefore(r.Context(), before)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "history-changed")
	w.WriteHeader(http.StatusNoContent)
}

// ranAt is when a query ran, as briefly as is clear: the time alone today.
func ranAt(t time.Time) string {
	now := time.Now()
	switch {
	case t.IsZero():
		return "—"
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("2 Jan 15:04")
	}
	return t.Format("2 Jan 2006")
}
