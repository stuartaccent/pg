package term

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The test binary runs itself as the command, so the tests need no shell.
func TestMain(m *testing.M) {
	switch os.Getenv("TERM_TEST_HELPER") {
	case "echo":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("got:%s\n", strings.TrimRight(line, "\r\n"))
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), "TERM_TEST_HELPER="+mode)
	return cmd
}

// readAll reads t until it ends, failing if that takes too long.
func readAll(t *testing.T, term Term) string {
	t.Helper()
	done := make(chan string)
	go func() {
		var out strings.Builder
		_, _ = io.Copy(&out, term)
		done <- out.String()
	}()
	select {
	case out := <-done:
		return out
	case <-time.After(20 * time.Second):
		t.Fatal("reading never ended")
		return ""
	}
}

func TestStart(t *testing.T) {
	ctx := t.Context()
	term, err := Start(ctx, helper(ctx, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Resize(100, 40); err != nil {
		t.Errorf("resize: %v", err)
	}
	if _, err := term.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- term.Wait() }()
	if out := readAll(t, term); !strings.Contains(out, "got:hello") {
		t.Errorf("output = %q", out)
	}
	if err := <-waited; err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd := helper(ctx, "sleep")
	cmd.WaitDelay = 5 * time.Second
	term, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	go func() { _, _ = io.Copy(io.Discard, term) }()
	waited := make(chan error, 1)
	go func() { waited <- term.Wait() }()
	cancel()
	select {
	case err := <-waited:
		if err == nil {
			t.Error("wait: no error for a stopped command")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelling didn't stop it")
	}
}

func TestStartMissing(t *testing.T) {
	ctx := t.Context()
	if _, err := Start(ctx, exec.CommandContext(ctx, "no-such-program-pg")); err == nil {
		t.Error("no error starting a program that isn't there")
	}
}
