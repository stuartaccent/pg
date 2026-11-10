// Command dev serves the UI on localhost for working on it in a normal
// browser, without rebuilding the Wails app. Pair with `templ generate --watch`.
package main

import (
	"log"
	"net/http"

	"pg/web"
)

const addr = "127.0.0.1:8080"

// localOnly refuses other Host headers. A site can point its own domain at this
// machine, which makes it look same-origin to the check above. It also refuses
// any request another site's page makes, even a GET: loading / starts over,
// closing every tab's session.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr && r.Host != "localhost:8080" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	srv := web.New()

	log.Printf("listening on http://%s", addr)
	// Unlike the desktop app this is reachable from the browser's other tabs,
	// so any website could otherwise POST SQL to the open connection.
	protected := localOnly(http.NewCrossOriginProtection().Handler(srv.Handler()))
	log.Fatal(http.ListenAndServe(addr, protected))
}
