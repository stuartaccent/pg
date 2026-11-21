//go:build windows || production

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// enableDevTools does nothing: Windows dev builds have devtools already, and
// releases have none.
func enableDevTools(*application.WebviewWindow) {}
