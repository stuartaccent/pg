// Command dev serves the UI on localhost for working on it in a normal
// browser, without rebuilding the Wails app. Pair with `templ generate --watch`.
package main

import (
	"log"
	"net/http"

	"pg/web"
)

func main() {
	srv := web.New()
	defer srv.Close()

	addr := "127.0.0.1:8080"
	log.Printf("listening on http://%s", addr)
	// Unlike the desktop app this is reachable from the browser's other tabs,
	// so any website could otherwise POST SQL to the open connection.
	protected := http.NewCrossOriginProtection().Handler(srv.Handler())
	log.Fatal(http.ListenAndServe(addr, protected))
}
