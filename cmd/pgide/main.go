// Command pgide is the desktop app: a Wails window around web.Handler.
package main

import (
	_ "embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"pg/web"
)

// icon is rendered from icon.svg, with rsvg-convert -w 1024 icon.svg -o icon.png.
//
//go:embed icon.png
var icon []byte

func main() {
	srv := web.New()

	app := application.New(application.Options{
		Name:       "pg",
		Icon:       icon,
		Assets:     application.AssetOptions{Handler: srv.Handler()},
		OnShutdown: srv.Close,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// Options.Icon alone doesn't reach the Dock: run outside an app bundle, macOS
	// puts the default tile back when launching finishes, so set it after that.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		app.SetIcon(icon)
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
