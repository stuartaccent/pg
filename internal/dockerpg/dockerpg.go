// Package dockerpg runs Postgres' own tools in Docker, so backups need nothing
// installed and always use the version that matches the server.
package dockerpg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pg/internal/conn"
)

const (
	sandboxLabel  = "pg.sandbox"
	sandboxPrefix = "pg-sandbox-"
	sandboxDB     = "sandbox"
	readyTimeout  = 90 * time.Second
)

// Image returns the image for a server's reported version, such as
// "17.2 (Debian 17.2-1)". pg_dump refuses a server newer than itself, so it
// has to match rather than being whatever's handy.
func Image(serverVersion string) string {
	major, _, _ := strings.Cut(serverVersion, ".")
	if _, err := strconv.Atoi(major); err != nil {
		return "postgres:latest"
	}
	return "postgres:" + major
}

var unsafeInName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// BackupName returns the file name for a new backup. It records the server's
// version, which is what a sandbox for it should run.
func BackupName(database, image string, at time.Time) string {
	version := strings.TrimPrefix(image, "postgres:")
	return fmt.Sprintf("%s-pg%s-%s.backup", unsafeInName.ReplaceAllString(database, "_"), version, at.Format("20060102-150405"))
}

// versionInName finds the version BackupName records, by the time after it, so
// a database called legacy-pg9-app doesn't count.
var versionInName = regexp.MustCompile(`-pg(\d+)-\d{8}-\d{6}`)

// ImageForBackup returns the image to restore a backup with. Postgres can
// restore older dumps, so a file from elsewhere gets the latest.
func ImageForBackup(name string) string {
	if m := versionInName.FindStringSubmatch(name); m != nil {
		return "postgres:" + m[1]
	}
	return "postgres:latest"
}

// dockerDirs are where Docker lands on a Mac. An app opened from the Finder
// doesn't get the shell's PATH, so these have to be added.
const dockerDirs = "/usr/local/bin:/opt/homebrew/bin"

// dockerPath finds docker, including in dockerDirs.
func dockerPath() string {
	if path, err := exec.LookPath("docker"); err == nil {
		return path
	}
	for dir := range strings.SplitSeq(dockerDirs, ":") {
		path := filepath.Join(dir, "docker")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return "docker"
}

// command prepares a docker command. Cancelling asks it to stop rather than
// killing it, as docker passes on a signal but a killed client leaves its
// container running.
func command(ctx context.Context, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, dockerPath(), args...)
	// docker runs its credential helper from the PATH, so finding docker isn't enough.
	cmd.Env = append(append(os.Environ(), "PATH="+os.Getenv("PATH")+":"+dockerDirs), env...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// output runs a docker command that finishes quickly and returns its output.
func output(ctx context.Context, env []string, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := command(ctx, env, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// run runs cmd, passing each line of its stderr to progress, and returns the
// last few lines, which hold the reason if it failed.
func run(cmd *exec.Cmd, progress func(string)) ([]string, error) {
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var last []string
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		progress(line)
		last = append(last, line)
		if len(last) > 5 {
			last = last[1:]
		}
	}
	// A line too long to scan stops the loop. The rest still has to be read,
	// or the command blocks writing it and never exits.
	_, _ = io.Copy(io.Discard, pipe)
	return last, cmd.Wait()
}

// containerHost is host as seen from inside a container, where localhost is
// the container itself.
func containerHost(host string) string {
	switch {
	case host == "localhost", host == "127.0.0.1", host == "::1":
		return "host.docker.internal"
	case strings.HasPrefix(host, "/"): // a socket, for a service with no host
		return "host.docker.internal"
	}
	return host
}

// pgEnv is how a container's Postgres tools reach s: libpq's variables, and
// docker's flags to pass them on. They go through the environment rather than
// the arguments, so the password stays out of the process list.
//
// shortcut: sslmode isn't passed on, so the tools use their default of prefer.
// Add it if a service ever needs verify-full.
func pgEnv(s conn.Settings) (env, pass []string) {
	env = []string{
		"PGHOST=" + containerHost(s.Host),
		"PGPORT=" + strconv.Itoa(int(s.Port)),
		"PGUSER=" + s.User,
		"PGPASSWORD=" + s.Password,
		"PGDATABASE=" + s.Database,
	}
	for _, v := range env {
		name, _, _ := strings.Cut(v, "=")
		pass = append(pass, "-e", name)
	}
	return env, pass
}

// Backup dumps a database to dest in pg_dump's custom format, without its rows
// if schemaOnly. It's written beside dest and moved there when complete, so a
// backup that exists is a backup that finished.
func Backup(ctx context.Context, s conn.Settings, image, dest string, schemaOnly bool, progress func(string)) error {
	partial := dest + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	env, pass := pgEnv(s)
	cmd := command(ctx, env, append(append([]string{"run", "--rm", "--init"}, pass...),
		image, "pg_dump", "--format=custom", "--verbose")...)
	if schemaOnly {
		cmd.Args = append(cmd.Args, "--schema-only")
	}
	cmd.Stdout = f
	last, err := run(cmd, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("backup failed: %w: %s", err, strings.Join(last, " "))
	}
	// Names go to the second in local time, which repeats when the clocks go
	// back, and a rename would replace the earlier backup without a word.
	if _, err := os.Lstat(dest); err == nil {
		_ = os.Remove(partial)
		return errors.New(filepath.Base(dest) + " already exists: back up again in a second")
	}
	return os.Rename(partial, dest)
}

// Psql prepares psql from image, interactively, on s. It needs a terminal for
// its stdin and stdout. It starts in dir, mounted read-only at /queries, so \i
// runs the files there by their paths in dir. The connection details go in
// docker's environment, so the password isn't in its arguments. If readOnly,
// transactions default to read-only, as a read-only service's query tabs do.
func Psql(ctx context.Context, s conn.Settings, readOnly bool, image, dir string) *exec.Cmd {
	env, pass := pgEnv(s)
	if readOnly {
		env, pass = append(env, "PGOPTIONS=-c default_transaction_read_only=on"), append(pass, "-e", "PGOPTIONS")
	}
	return command(ctx, env, append(append([]string{"run", "--rm", "-it", "--init"}, pass...),
		"-v", dir+":/queries:ro", "-w", "/queries", image, "psql")...)
}

// Sandbox is a throwaway Postgres in a container, holding a restored backup.
type Sandbox struct {
	Name     string
	Source   string // the backup it was made from
	Port     int
	Password string
}

// removeLater removes a sandbox with a context of its own, so it works even
// once the job's has ended.
func removeLater(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = Remove(ctx, name)
}

// IsSandbox reports whether a service name is a sandbox's.
func IsSandbox(name string) bool {
	return strings.HasPrefix(name, sandboxPrefix)
}

// URL is how other programs can connect to it, from this machine only.
func (s Sandbox) URL() string {
	u := url.URL{Scheme: "postgresql", User: url.UserPassword("postgres", s.Password), Host: fmt.Sprintf("127.0.0.1:%d", s.Port), Path: "/" + sandboxDB}
	return u.String()
}

// ServiceEntry is a pg_service.conf section for it, to paste in by hand.
// pg never writes the service file itself.
func (s Sandbox) ServiceEntry() string {
	return fmt.Sprintf("[%s]\nhost=127.0.0.1\nport=%d\ndbname=%s\nuser=postgres\npassword=%s\nsslmode=disable\n", s.Name, s.Port, sandboxDB, s.Password)
}

// Database is the name of the database a sandbox holds the backup in.
func (Sandbox) Database() string { return sandboxDB }

// DSN returns how to connect to it.
func (s Sandbox) DSN() string {
	return fmt.Sprintf("host=127.0.0.1 port=%d user=postgres password=%s dbname=%s sslmode=disable", s.Port, s.Password, sandboxDB)
}

var publishedPort = regexp.MustCompile(`127\.0\.0\.1:(\d+)->5432`)

// parseSandboxes reads the output of the docker ps in Sandboxes.
func parseSandboxes(out string) []Sandbox {
	var found []Sandbox
	for line := range strings.Lines(out) {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 4 {
			continue
		}
		m := publishedPort.FindStringSubmatch(fields[3])
		if m == nil {
			continue // still starting, or going
		}
		port, _ := strconv.Atoi(m[1])
		found = append(found, Sandbox{Name: fields[0], Password: fields[1], Source: fields[2], Port: port})
	}
	return found
}

// Sandboxes lists the running sandboxes. Docker is where they're recorded, so
// they outlive the app and can't be left running unnoticed.
func Sandboxes(ctx context.Context) ([]Sandbox, error) {
	out, err := output(ctx, nil, "ps", "--filter", "label="+sandboxLabel,
		"--format", `{{.Names}}	{{.Label "pg.sandbox.password"}}	{{.Label "pg.sandbox.source"}}	{{.Ports}}`)
	if err != nil {
		return nil, err
	}
	return parseSandboxes(out), nil
}

// Remove deletes a sandbox and everything in it.
func Remove(ctx context.Context, name string) error {
	_, err := output(ctx, nil, "rm", "--force", "--volumes", name)
	return err
}

var ignoredErrors = regexp.MustCompile(`errors ignored on restore: (\d+)`)

// Skipped is what a restore got past. A few are normal for a real database,
// such as roles that exist on the source server and not here.
type Skipped struct {
	Count int
	First string // the first error, if it could be picked out
}

var serverError = regexp.MustCompile(`ERROR:\s+(.+)`)

// runRestore runs a pg_restore, which exits non-zero for skipped errors as well
// as real failures. It counts the skipped ones for us, which tells them apart.
func runRestore(ctx context.Context, cmd *exec.Cmd, progress func(string)) (Skipped, error) {
	var skipped Skipped
	last, err := run(cmd, func(line string) {
		if m := ignoredErrors.FindStringSubmatch(line); m != nil {
			skipped.Count, _ = strconv.Atoi(m[1])
		}
		if m := serverError.FindStringSubmatch(line); m != nil && skipped.First == "" {
			skipped.First = m[1]
		}
		progress(line)
	})
	if err != nil && (skipped.Count == 0 || ctx.Err() != nil) {
		return skipped, fmt.Errorf("restore failed: %w: %s", err, strings.Join(last, " "))
	}
	return skipped, nil
}

// NewSandbox starts a sandbox and restores backup into it, returning what the
// restore skipped over.
//
// The port is on 127.0.0.1 only, and has a random password, as it holds a copy
// of real data.
func NewSandbox(ctx context.Context, image, backup string, progress func(string)) (Sandbox, Skipped, error) {
	f, err := os.Open(backup)
	if err != nil {
		return Sandbox{}, Skipped{}, err
	}
	defer f.Close()

	id := make([]byte, 4)
	password := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Sandbox{}, Skipped{}, err
	}
	if _, err := rand.Read(password); err != nil {
		return Sandbox{}, Skipped{}, err
	}
	sb := Sandbox{
		Name:     sandboxPrefix + hex.EncodeToString(id),
		Source:   filepath.Base(backup),
		Password: hex.EncodeToString(password),
	}

	progress("Starting " + image)
	start := command(ctx, []string{"POSTGRES_PASSWORD=" + sb.Password, "POSTGRES_DB=" + sandboxDB},
		"run", "--detach", "--rm", "--name", sb.Name,
		"--label", sandboxLabel, "--label", "pg.sandbox.password="+sb.Password, "--label", "pg.sandbox.source="+sb.Source,
		"-e", "POSTGRES_PASSWORD", "-e", "POSTGRES_DB",
		"--publish", "127.0.0.1::5432", image)
	if last, err := run(start, progress); err != nil {
		return Sandbox{}, Skipped{}, fmt.Errorf("starting the sandbox: %w: %s", err, strings.Join(last, " "))
	}

	// A sandbox that didn't finish isn't one anyone should be left with.
	progress("Waiting for Postgres to start")
	if err := waitUntilReady(ctx, sb.Name); err != nil {
		removeLater(sb.Name)
		return Sandbox{}, Skipped{}, err
	}
	out, err := output(ctx, nil, "port", sb.Name, "5432/tcp")
	if err == nil {
		sb.Port, err = portOf(out)
	}
	if err != nil {
		removeLater(sb.Name)
		return Sandbox{}, Skipped{}, err
	}

	progress("Restoring " + sb.Source)
	restore := command(ctx, nil, "exec", "--interactive", sb.Name,
		"pg_restore", "--no-owner", "--no-privileges", "--verbose", "--username=postgres", "--dbname="+sandboxDB)
	restore.Stdin = f
	skipped, err := runRestore(ctx, restore, progress)
	if err != nil {
		removeLater(sb.Name)
		return Sandbox{}, Skipped{}, err
	}
	return sb, skipped, nil
}

// portOf reads a port from docker port's output, such as "127.0.0.1:55012".
func portOf(out string) (int, error) {
	first, _, _ := strings.Cut(out, "\n")
	_, port, ok := strings.Cut(strings.TrimSpace(first), ":")
	if !ok {
		return 0, fmt.Errorf("unexpected docker port output: %q", out)
	}
	return strconv.Atoi(port)
}

// waitUntilReady waits for the container's real server. It asks over TCP, as
// the image's first-run server only listens on a socket and then restarts.
func waitUntilReady(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for {
		cmd := command(ctx, nil, "exec", name, "pg_isready", "--host=127.0.0.1", "--username=postgres", "--dbname="+sandboxDB)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if cmd.Run() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("the sandbox's Postgres didn't start in time")
		case <-time.After(300 * time.Millisecond):
		}
	}
}
