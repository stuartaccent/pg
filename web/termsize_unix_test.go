//go:build !windows

package web

import (
	"os"

	"github.com/creack/pty"
)

// termSize is the size of the terminal psql's on, as stty size gives it.
func termSize() (rows, cols int) {
	ws, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return 0, 0
	}
	return int(ws.Rows), int(ws.Cols)
}
