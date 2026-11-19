//go:build windows

package web

import (
	"os"

	"golang.org/x/sys/windows"
)

// termSize is the size of the console psql's on, as stty size gives it.
func termSize() (rows, cols int) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info); err != nil {
		return 0, 0
	}
	return int(info.Window.Bottom - info.Window.Top + 1), int(info.Window.Right - info.Window.Left + 1)
}
