// Package term runs a command on a pseudo-terminal, for psql's tab: a pty on
// macOS and Linux, a pseudo console (ConPTY) on Windows.
package term

import "io"

// Term is a command running on a pseudo-terminal. Reading gets what it
// writes, and writing types into it.
type Term interface {
	io.ReadWriteCloser
	// Resize tells the command the terminal's size, in characters.
	Resize(cols, rows uint16) error
	// Wait waits for the command to exit. After that, reads drain what's left
	// and then fail.
	Wait() error
}
