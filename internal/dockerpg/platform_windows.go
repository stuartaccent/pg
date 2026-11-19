//go:build windows

package dockerpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// computer is what the user's machine is called in messages.
const computer = "PC"

// exe is the suffix of a program's file name.
const exe = ".exe"

// dockerDirs are where Docker Desktop puts docker, in case it isn't on the
// PATH an app started from the Start menu gets.
var dockerDirs = []string{filepath.Join(programFiles(), "Docker", "Docker", "resources", "bin")}

// psqlDirs are where the EDB installer puts psql and pg_dump, one folder per
// version, which it doesn't add to the PATH unless asked. A variable for the
// tests.
var psqlDirs = func() string {
	dirs, _ := filepath.Glob(filepath.Join(programFiles(), "PostgreSQL", "*", "bin"))
	return strings.Join(dirs, string(os.PathListSeparator))
}()

func programFiles() string {
	if dir := os.Getenv("ProgramFiles"); dir != "" {
		return dir
	}
	return `C:\Program Files`
}

// nameContainer names the container a `docker run` makes, so it can be
// stopped by name. Windows has no signal for the docker client to pass on,
// and killing the client leaves its container running.
func nameContainer(args []string) ([]string, string) {
	if len(args) == 0 || args[0] != "run" || slices.Contains(args, "--name") || slices.Contains(args, "--detach") {
		return args, ""
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "pg-run-" + hex.EncodeToString(b)
	return append([]string{"run", "--name", name}, args[1:]...), name
}

// stopper stops the container by name, as a signal would on Unix, and lets
// the client exit once it has. With no container to stop, or if docker can't
// stop it, the client is killed.
func stopper(cmd *exec.Cmd, container string) func() error {
	return func() error {
		if container != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stop := exec.CommandContext(ctx, dockerPath(), "kill", "--signal=TERM", container)
			hideWindow(stop)
			if stop.Run() == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
}

// hideWindow keeps a console program from opening a window of its own, as
// Windows gives one to each started from an app that has none.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}
