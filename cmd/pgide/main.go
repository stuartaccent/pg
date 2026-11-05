// Command pgide is the desktop app: a Wails window around web.Handler.
package main

import (
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"

	"pg/web"
)

func main() {
	srv := web.New()

	app := application.New(application.Options{
		Name:       "pg",
		Assets:     application.AssetOptions{Handler: srv.Handler()},
		OnShutdown: srv.Close,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "pg",
		Width:  1200,
		Height: 800,
		URL:    "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
