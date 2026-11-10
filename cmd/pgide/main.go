// Command pgide is the desktop app: a Wails window around web.Handler.
package main

import (
	_ "embed"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"pg/web"
)

// icon is rendered from icon.svg, with rsvg-convert -w 1024 icon.svg -o icon.png.
//
//go:embed icon.png
var icon []byte

// The page's background, so a window has no white flash while it loads.
var (
	lightBackground = application.NewRGB(244, 245, 247)
	darkBackground  = application.NewRGB(16, 18, 21)
)

func main() {
	srv := web.New()

	var app *application.App
	var quitting, asking atomic.Bool
	// mayQuit asks first if a job is running, or a tab has a transaction open.
	// AppKit wants an answer straight away on the main thread, which a dialog
	// can't give, so the answer is no and the dialog's Quit button starts
	// quitting again.
	mayQuit := func() bool {
		job, open := srv.RunningJob(), srv.OpenTransactions()
		if job == "" && open == 0 || quitting.Load() {
			return true
		}
		if asking.CompareAndSwap(false, true) {
			go askToQuit(app, job, open, &quitting, &asking)
		}
		return false
	}

	app = application.New(application.Options{
		Name:       "pg",
		Icon:       icon,
		Assets:     application.AssetOptions{Handler: srv.Handler()},
		OnShutdown: srv.Close,
		ShouldQuit: mayQuit,
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})
	app.Menu.Set(appMenu(app))

	srv.ChooseFile = func(dir, name string) (string, error) {
		ext := filepath.Ext(name)
		dialog := app.Dialog.SaveFile().
			SetFilename(name).
			AddFilter(strings.ToUpper(ext[1:]), "*"+ext).
			CanCreateDirectories(true)
		if dir != "" {
			dialog.SetDirectory(dir)
		}
		return dialog.PromptForSingleSelection()
	}

	srv.OpenFile = func(dir string) (string, error) {
		return app.Dialog.OpenFile().
			SetDirectory(dir).
			AddFilter("SQL", "*.sql").
			CanChooseFiles(true).
			PromptForSingleSelection()
	}

	// Options.Icon alone doesn't reach the Dock: run outside an app bundle, macOS
	// puts the default tile back when launching finishes, so set it after that.
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		app.SetIcon(icon)

		// Made here as the system's appearance can't be read before the app starts.
		background := lightBackground
		if app.Env.IsDarkMode() {
			background = darkBackground
		}
		window := app.Window.NewWithOptions(application.WebviewWindowOptions{
			Title:            "pg",
			Width:            1200,
			Height:           800,
			MinWidth:         900,
			MinHeight:        600,
			BackgroundColour: background,
			URL:              "/",
		})
		// Closing the last window quits, and by the time that's asked the window
		// has gone, so it has to be stopped here.
		window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
			if !mayQuit() {
				event.Cancel()
			}
		})
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// askToQuit checks that a running job should be stopped, then quits.
func askToQuit(app *application.App, job string, open int, quitting, asking *atomic.Bool) {
	title, message, quit := job+" is still running", "Quitting stops it and tidies up what it had started.", "Stop and quit"
	if job == "" {
		title, message, quit = "A tab has an open transaction", "Quitting rolls it back.", "Roll back and quit"
		if open > 1 {
			title = fmt.Sprintf("%d tabs have open transactions", open)
		}
	} else if open > 0 {
		message += " It also rolls back the open transaction in a tab."
	}
	dialog := app.Dialog.Question().SetTitle(title).SetMessage(message)
	dialog.AddButton(quit).OnClick(func() {
		quitting.Store(true)
		asking.Store(false)
		go app.Quit()
	})
	keep := dialog.AddButton("Keep running").OnClick(func() { asking.Store(false) })
	dialog.SetDefaultButton(keep).SetCancelButton(keep).Show()
}

// appMenu is the standard menus, with File running the page's own commands.
func appMenu(app *application.App) *application.Menu {
	menu := app.Menu.New()
	menu.AddRole(application.AppMenu)
	file := menu.AddSubmenu("File")
	add := func(label, accelerator, action string) {
		item := file.Add(label).OnClick(func(*application.Context) {
			if window := app.Window.Current(); window != nil {
				window.ExecJS("fromMenu(" + action + ")")
			}
		})
		if accelerator != "" {
			item.SetAccelerator(accelerator)
		}
	}
	add("New Tab", "CmdOrCtrl+T", "newTab")
	add("Close Tab", "CmdOrCtrl+W", "closeActiveTab")
	add("Close All Tabs", "", "closeAllTabs")
	file.AddSeparator()
	add("Save Query…", "CmdOrCtrl+S", "saveQuery")
	add("Saved Queries…", "", "showQueries")
	add("Open Query File…", "CmdOrCtrl+O", "openQueryFile")
	add("History", "", "showHistory")
	file.AddSeparator()
	add("Activity", "", "showActivity")
	add("Health", "", "showHealth")
	add("Backups", "", "showBackups")
	menu.AddRole(application.EditMenu)
	menu.AddRole(application.ViewMenu)
	menu.AddRole(application.WindowMenu)
	menu.AddRole(application.HelpMenu)
	return menu
}
