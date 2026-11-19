package dockerpg

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Windows names a docker run's container, to stop it by name.
func TestNameContainer(t *testing.T) {
	args, name := nameContainer([]string{"run", "--rm", "postgres:18", "psql"})
	if runtime.GOOS != "windows" {
		if name != "" || !slices.Equal(args, []string{"run", "--rm", "postgres:18", "psql"}) {
			t.Errorf("named elsewhere: %v, %q", args, name)
		}
		return
	}
	if !strings.HasPrefix(name, "pg-run-") || !slices.Equal(args, []string{"run", "--name", name, "--rm", "postgres:18", "psql"}) {
		t.Errorf("run = %v, %q", args, name)
	}
	for _, other := range [][]string{
		{"run", "--detach", "--name", "pg-sandbox-x", "postgres:18"},
		{"exec", "pg-sandbox-x", "pg_isready"},
	} {
		if got, name := nameContainer(other); name != "" || !slices.Equal(got, other) {
			t.Errorf("%v named: %v, %q", other, got, name)
		}
	}
}
