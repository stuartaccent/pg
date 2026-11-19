//go:build !windows

package web

import "os/exec"

// fileManager is what the system's file browser is called.
const fileManager = "Finder"

// editFile opens path in the user's text editor.
func editFile(path string) error { return exec.Command("open", "-t", path).Run() }

// showFolder opens path in the Finder.
func showFolder(path string) error { return exec.Command("open", path).Run() }
