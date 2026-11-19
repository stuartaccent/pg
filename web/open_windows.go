//go:build windows

package web

import "os/exec"

// fileManager is what the system's file browser is called.
const fileManager = "File Explorer"

// editFile opens path in Notepad. The config files end .conf, which Windows
// has no app for, so asking for the file's own app would only ask the user
// which to use.
func editFile(path string) error { return start(exec.Command("notepad.exe", path)) }

// showFolder opens path in File Explorer.
func showFolder(path string) error { return start(exec.Command("explorer.exe", path)) }

// start starts cmd without waiting for it, as these stay open until the user
// closes them, unlike macOS's open. explorer.exe also exits 1 when it works.
func start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
