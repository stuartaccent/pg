//go:build !windows

package dockerpg

import (
	"os/exec"
	"syscall"
)

// computer is what the user's machine is called in messages.
const computer = "Mac"

// exe is the suffix of a program's file name.
const exe = ""

// dockerDirs are where Docker lands on a Mac. An app opened from the Finder
// doesn't get the shell's PATH, so these have to be added.
var dockerDirs = []string{"/usr/local/bin", "/opt/homebrew/bin"}

// psqlDirs are where psql and pg_dump land on a Mac but an app opened from the Finder
// won't look: Homebrew's libpq, which it doesn't link, its bin, and
// Postgres.app. A variable for the tests.
var psqlDirs = "/opt/homebrew/opt/libpq/bin:/opt/homebrew/bin:/usr/local/opt/libpq/bin:/usr/local/bin:/Applications/Postgres.app/Contents/Versions/latest/bin"

// nameContainer returns docker's arguments as they are: a signal reaches the
// container through the docker client, so it needn't be named to be stopped.
func nameContainer(args []string) ([]string, string) { return args, "" }

// stopper asks cmd to stop rather than killing it, as docker passes on a
// signal but a killed client leaves its container running.
func stopper(cmd *exec.Cmd, _ string) func() error {
	return func() error { return cmd.Process.Signal(syscall.SIGTERM) }
}

// hideWindow does nothing: only Windows opens a window for a console program.
func hideWindow(*exec.Cmd) {}
