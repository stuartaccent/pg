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

var versionInName = regexp.MustCompile(`-pg(\d+)-`)

// ImageForBackup returns the image to restore a backup with. Postgres can
// restore older dumps, so a file from elsewhere gets the latest.
func ImageForBackup(name string) string {
	if m := versionInName.FindStringSubmatch(name); m != nil {
		return "postgres:" + m[1]
	}
	return "postgres:latest"
}

// dockerPath finds docker, including where it lands on a Mac, as an app opened
// from the Finder doesn't get the shell's PATH.
func dockerPath() string {
	if path, err := exec.LookPath("docker"); err == nil {
		return path
	}
	for _, path := range []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker"} {
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
	cmd.Env = append(os.Environ(), env...)
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
	return last, cmd.Wait()
}

// containerHost is host as seen from inside a container, where localhost is
// the container itself.
func containerHost(host string) string {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return "host.docker.internal"
	}
	return host
}

// Backup dumps a database to dest in pg_dump's custom format. The connection
// details go through the environment, so the password stays out of the process
// list. It's written beside dest and moved there when complete, so a backup
// that exists is a backup that finished.
//
// shortcut: sslmode isn't passed on, so pg_dump uses its default of prefer. Add
// it if a service ever needs verify-full.
func Backup(ctx context.Context, s conn.Settings, image, dest string, progress func(string)) error {
	partial := dest + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := command(ctx, []string{
		"PGHOST=" + containerHost(s.Host),
		"PGPORT=" + strconv.Itoa(int(s.Port)),
		"PGUSER=" + s.User,
		"PGPASSWORD=" + s.Password,
		"PGDATABASE=" + s.Database,
	}, "run", "--rm", "--init",
		"-e", "PGHOST", "-e", "PGPORT", "-e", "PGUSER", "-e", "PGPASSWORD", "-e", "PGDATABASE",
		image, "pg_dump", "--format=custom", "--verbose")
	cmd.Stdout = f
	last, err := run(cmd, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("backup failed: %w: %s", err, strings.Join(last, " "))
	}
	return os.Rename(partial, dest)
}

// Sandbox is a throwaway Postgres in a container, holding a restored backup.
type Sandbox struct {
	Name     string
	Source   string // the backup it was made from
	Port     int
	Password string
}

// IsSandbox reports whether a service name is a sandbox's.
func IsSandbox(name string) bool {
	return strings.HasPrefix(name, sandboxPrefix)
}

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

// NewSandbox starts a sandbox and restores backup into it, returning how many
// errors the restore skipped over. A few are normal for a real database, such
// as roles that exist on the source server and not here.
//
// The port is on 127.0.0.1 only, and has a random password, as it holds a copy
// of real data.
func NewSandbox(ctx context.Context, image, backup string, progress func(string)) (Sandbox, int, error) {
	f, err := os.Open(backup)
	if err != nil {
		return Sandbox{}, 0, err
	}
	defer f.Close()

	id := make([]byte, 4)
	password := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Sandbox{}, 0, err
	}
	if _, err := rand.Read(password); err != nil {
		return Sandbox{}, 0, err
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
		return Sandbox{}, 0, fmt.Errorf("starting the sandbox: %w: %s", err, strings.Join(last, " "))
	}

	// A sandbox that didn't finish isn't one anyone should be left with. It's
	// removed even if ctx is what ended it.
	discard := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = Remove(cleanup, sb.Name)
	}

	progress("Waiting for Postgres to start")
	if err := waitUntilReady(ctx, sb.Name); err != nil {
		discard()
		return Sandbox{}, 0, err
	}
	out, err := output(ctx, nil, "port", sb.Name, "5432/tcp")
	if err == nil {
		sb.Port, err = portOf(out)
	}
	if err != nil {
		discard()
		return Sandbox{}, 0, err
	}

	progress("Restoring " + sb.Source)
	restore := command(ctx, nil, "exec", "--interactive", sb.Name,
		"pg_restore", "--no-owner", "--no-privileges", "--verbose", "--username=postgres", "--dbname="+sandboxDB)
	restore.Stdin = f
	var ignored int
	last, err := run(restore, func(line string) {
		if m := ignoredErrors.FindStringSubmatch(line); m != nil {
			ignored, _ = strconv.Atoi(m[1])
		}
		progress(line)
	})
	// pg_restore exits non-zero for skipped errors too, which it counts for us.
	if err != nil && (ignored == 0 || ctx.Err() != nil) {
		discard()
		return Sandbox{}, 0, fmt.Errorf("restore failed: %w: %s", err, strings.Join(last, " "))
	}
	return sb, ignored, nil
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
