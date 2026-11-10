package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	handler := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for host, want := range map[string]int{
		"127.0.0.1:8080":    http.StatusOK,
		"localhost:8080":    http.StatusOK,
		"evil.example:8080": http.StatusForbidden,
		"evil.example":      http.StatusForbidden,
		"":                  http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodPost, "/query", nil)
		req.Host = host
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Host %q: status = %d, want %d", host, w.Code, want)
		}
	}
}

func TestLocalOnlyRefusesOtherSites(t *testing.T) {
	handler := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for site, want := range map[string]int{"": http.StatusOK, "same-origin": http.StatusOK, "none": http.StatusOK, "cross-site": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = addr
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("Sec-Fetch-Site %q: status = %d, want %d", site, w.Code, want)
		}
	}
}
