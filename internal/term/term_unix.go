//go:build !windows

package term

import (
	"context"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixTerm struct {
	*os.File
	cmd *exec.Cmd
}

// Start starts cmd on a new pty. ctx is for Windows, which needs telling:
// here cmd's own context stops it.
func Start(_ context.Context, cmd *exec.Cmd) (Term, error) {
	f, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &unixTerm{File: f, cmd: cmd}, nil
}

func (t *unixTerm) Resize(cols, rows uint16) error {
	return pty.Setsize(t.File, &pty.Winsize{Cols: cols, Rows: rows})
}

func (t *unixTerm) Wait() error { return t.cmd.Wait() }
