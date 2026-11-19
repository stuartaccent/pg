package dockerpg

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for Postgres' tools, and for the shell, so the
// tests run the same on every system. As a tool it's linked into a folder
// under the tool's name, and goes by the name it's run as.
func TestMain(m *testing.M) {
	// A helper's checked first, as one started in a test with a fake on the
	// PATH has DOCKERPG_FAKE set too.
	if mode := os.Getenv("DOCKERPG_HELPER"); mode != "" {
		os.Exit(helper(mode))
	}
	if os.Getenv("DOCKERPG_FAKE") != "" {
		os.Exit(fakeTool(strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"), os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeTool(name string, args []string) int {
	switch name {
	case "psql":
		if len(args) > 0 && args[0] == "--version" {
			fmt.Printf("psql (PostgreSQL) %s.2 (Homebrew)\n", os.Getenv("FAKE_PSQL_VERSION"))
		}
	case "pg_dump":
		// pg_dump 18, much shortened. It notes how it was run in ran.
		if len(args) > 0 && args[0] == "--version" {
			fmt.Println("pg_dump (PostgreSQL) 18.1")
			return 0
		}
		ran := fmt.Sprintf("%s %s %s %s\n", strings.Join(args, " "), os.Getenv("PGHOST"), os.Getenv("PGPASSWORD"), os.Getenv("PGSERVICE"))
		if err := os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "ran"), []byte(ran), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if len(args) > 2 && args[2] == "--table=none" {
			fmt.Print("--\n-- PostgreSQL database dump\n--\n\nSET statement_timeout = 0;\n")
			return 0
		}
		fmt.Print(fakeDump)
	default:
		fmt.Fprintln(os.Stderr, "no fake for", name)
		return 1
	}
	return 0
}

// helper does what a test's command would, as DOCKERPG_HELPER says.
func helper(mode string) int {
	switch mode {
	case "long-line": // 2 MB on one line, more than the scanner takes, and than a pipe holds
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", 2_000_000)+"\ndone\n")
	case "stderr": // writes DOCKERPG_STDERR, then sleeps and exits as told
		_, _ = io.WriteString(os.Stderr, os.Getenv("DOCKERPG_STDERR"))
		if d, err := time.ParseDuration(os.Getenv("DOCKERPG_SLEEP")); err == nil {
			time.Sleep(d)
		}
		code, _ := strconv.Atoi(os.Getenv("DOCKERPG_EXIT"))
		return code
	}
	return 0
}

// helperCmd runs the test binary as helper, with these settings.
func helperCmd(ctx context.Context, mode string, env ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(append(os.Environ(), "DOCKERPG_HELPER="+mode), env...)
	return cmd
}

// installFake puts the test binary in dir as the tool called name, and
// returns its path.
func installFake(t *testing.T, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+exe)
	if err := os.Link(self, path); err != nil { // a different drive, say
		copyFile(t, self, path)
	}
	t.Setenv("DOCKERPG_FAKE", "1")
	return path
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// withoutName drops the name Windows gives a container so it can be stopped,
// leaving docker's arguments as they are elsewhere.
func withoutName(args []string) []string {
	if i := slices.Index(args, "--name"); i >= 0 && i+1 < len(args) && strings.HasPrefix(args[i+1], "pg-run-") {
		return slices.Concat(args[:i], args[i+2:])
	}
	return args
}

const fakeDump = `--
-- PostgreSQL database dump
--

\restrict abc

SET statement_timeout = 0;
SELECT pg_catalog.set_config('search_path', '', false);

--
-- Name: t; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.t (
    id integer,
    status text DEFAULT 'draft
-- not yet sent

SET by the mailer'::text
);


ALTER TABLE public.t OWNER TO postgres;

--
-- Name: TABLE t; Type: COMMENT; Schema: public; Owner: postgres
--

COMMENT ON TABLE public.t IS 'Statuses:
\restrict
-- draft



end';

\unrestrict abc

`
