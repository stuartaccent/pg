//go:build windows

package term

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var updateProcThreadAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

// winTerm is a command on a pseudo console. ConPTY reads what's typed from
// one pipe and writes the screen, as VT sequences, to another, much as a pty
// does on Unix.
type winTerm struct {
	ctx     context.Context
	cmd     *exec.Cmd
	in      *os.File // what's typed
	out     *os.File // what the command writes
	console windows.Handle

	closeOnce, consoleOnce sync.Once
}

// Start starts cmd on a new pseudo console. ctx stops it as cmd's own context
// would, with cmd.Cancel if it has one, which Unix's exec.Cmd does by itself.
// cmd.Stdin, Stdout and Stderr are ignored: the console is all three.
func Start(ctx context.Context, cmd *exec.Cmd) (Term, error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	if cmd.Process != nil {
		return nil, errors.New("term: already started")
	}

	// The console reads inRead and writes outWrite; this side has the others.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		closeHandles(inRead, inWrite)
		return nil, err
	}
	var console windows.Handle
	err := windows.CreatePseudoConsole(windows.Coord{X: 80, Y: 25}, inRead, outWrite, 0, &console)
	// The console has its own copies now.
	closeHandles(inRead, outWrite)
	if err != nil {
		closeHandles(inWrite, outRead)
		return nil, err
	}
	t := &winTerm{
		ctx:     ctx,
		cmd:     cmd,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
		console: console,
	}
	if err := t.start(); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

// start creates the process attached to the console, and sets cmd.Process so
// cmd.Cancel can use it.
func (t *winTerm) start() error {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attrs.Delete()
	// The attribute's value is the console handle itself, not a pointer to it,
	// which attrs.Update would want, so this calls Windows directly.
	r, _, err := updateProcThreadAttribute.Call(uintptr(unsafe.Pointer(attrs.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(t.console), unsafe.Sizeof(t.console), 0, 0)
	if r == 0 {
		return err
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrs.List()
	// Without this a child of a process with redirected standard handles, such
	// as `go run` in a terminal, writes there rather than to the console.
	si.Flags = windows.STARTF_USESTDHANDLES

	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{t.cmd.Path}, t.cmd.Args[1:]...)))
	if err != nil {
		return err
	}
	var dir *uint16
	if t.cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(t.cmd.Dir); err != nil {
			return err
		}
	}
	env := t.cmd.Env
	if env == nil {
		env = os.Environ()
	}

	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, line, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		envBlock(env), dir, &si.StartupInfo, &pi)
	if err != nil {
		return &os.PathError{Op: "fork/exec", Path: t.cmd.Path, Err: err}
	}
	defer closeHandles(pi.Thread, pi.Process) // after FindProcess has its own
	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		return err
	}
	t.cmd.Process = proc
	return nil
}

func (t *winTerm) Read(p []byte) (int, error)  { return t.out.Read(p) }
func (t *winTerm) Write(p []byte) (int, error) { return t.in.Write(p) }

func (t *winTerm) Resize(cols, rows uint16) error {
	return windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait waits for the command, stopping it if ctx ends first, then closes the
// console so reads end once they've had the rest of its output.
func (t *winTerm) Wait() error {
	exited := make(chan struct{})
	go func() {
		select {
		case <-t.ctx.Done():
		case <-exited:
			return
		}
		if t.cmd.Cancel != nil {
			_ = t.cmd.Cancel()
		} else {
			_ = t.cmd.Process.Kill()
		}
		if t.cmd.WaitDelay > 0 {
			select {
			case <-time.After(t.cmd.WaitDelay):
				_ = t.cmd.Process.Kill()
			case <-exited:
			}
		}
	}()
	state, err := t.cmd.Process.Wait()
	close(exited)
	t.cmd.ProcessState = state
	t.closeConsole()
	if err != nil {
		return err
	}
	if !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}
	return nil
}

// closeConsole closes the pseudo console. Until it's closed reads never end,
// even once the command has exited. Some versions of Windows block closing it
// until its output has been read, so reads must still be going, or Close
// must have started draining them.
func (t *winTerm) closeConsole() {
	t.consoleOnce.Do(func() { windows.ClosePseudoConsole(t.console) })
}

// Close stops typing into the console and lets it go, draining what it still
// has to write so closing it can't block. Don't read after closing.
func (t *winTerm) Close() error {
	t.closeOnce.Do(func() {
		_ = t.in.Close()
		go func() {
			_, _ = io.Copy(io.Discard, t.out)
			_ = t.out.Close()
		}()
		go t.closeConsole()
	})
	return nil
}

func closeHandles(handles ...windows.Handle) {
	for _, h := range handles {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
	}
}

// envBlock is env as CreateProcess wants it: each "name=value" ending in a
// NUL, then another NUL. Names are case-insensitive on Windows and the last
// of a name wins, as with exec.Cmd. SYSTEMROOT is kept, as some of Windows'
// own libraries fail without it.
func envBlock(env []string) *uint16 {
	seen := map[string]int{}
	var kept []string
	for _, kv := range env {
		if strings.IndexByte(kv, 0) >= 0 {
			continue
		}
		name, _, ok := strings.Cut(kv[min(1, len(kv)):], "=") // a leading "=" is part of names like "=C:"
		if !ok {
			continue
		}
		key := strings.ToUpper(kv[:min(1, len(kv))] + name)
		if i, ok := seen[key]; ok {
			kept[i] = kv
			continue
		}
		seen[key] = len(kept)
		kept = append(kept, kv)
	}
	if _, ok := seen["SYSTEMROOT"]; !ok {
		if root := os.Getenv("SYSTEMROOT"); root != "" {
			kept = append(kept, "SYSTEMROOT="+root)
		}
	}
	var block []uint16
	for _, kv := range kept {
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	block = append(block, 0)
	return &block[0]
}
