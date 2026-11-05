package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, s *Server, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(method, url, nil))
	return w
}

func TestResponsesAreNotCached(t *testing.T) {
	for _, url := range []string{"/", "/tree?kind=schemas", "/static/app.css"} {
		if got := get(t, New(), http.MethodGet, url).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", url, got)
		}
	}
}

func TestNotConnected(t *testing.T) {
	s := New()
	if got := get(t, s, http.MethodGet, "/tree?kind=schemas").Code; got != http.StatusNoContent {
		t.Errorf("tree status = %d", got)
	}
	if got := get(t, s, http.MethodGet, "/definition?def=view&id=1").Code; got != http.StatusConflict {
		t.Errorf("definition status = %d", got)
	}
	if got := get(t, s, http.MethodGet, "/completions").Body.String(); strings.TrimSpace(got) != "{}" {
		t.Errorf("completions = %q", got)
	}
}

func TestNewTabsGetTheirOwnIDs(t *testing.T) {
	s := New()
	get(t, s, http.MethodGet, "/")
	body := get(t, s, http.MethodPost, "/tabs").Body.String()
	for _, want := range []string{`data-tab="2"`, `data-tab-button="2"`, `hx-swap-oob="beforeend:#tabs"`, `#results-2`} {
		if !strings.Contains(body, want) {
			t.Errorf("new tab lacks %q", want)
		}
	}
	if body := get(t, s, http.MethodPost, "/tabs").Body.String(); !strings.Contains(body, `data-tab="3"`) {
		t.Error("second new tab did not get the next id")
	}
}

// htmx appends the input's own id, and the first value wins, so the URL must
// not carry an empty one.
func TestSearchURLLeavesIDToTheInput(t *testing.T) {
	if body := get(t, New(), http.MethodGet, "/").Body.String(); !strings.Contains(body, `hx-get="/tree?kind=search"`) {
		t.Error("search input's URL should have no id")
	}
}

func TestReadOnlyBadge(t *testing.T) {
	for readOnly, want := range map[bool]bool{true: true, false: false} {
		var b strings.Builder
		if err := ConnectStatus("local", readOnly, nil).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(b.String(), "read-only"); got != want {
			t.Errorf("readOnly = %v: badge shown = %v", readOnly, got)
		}
	}
}
