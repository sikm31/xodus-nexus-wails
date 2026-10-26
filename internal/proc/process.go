// Package proc ports process.ts: tracked child processes with bounded output
// capture, line observation and backend diagnostics.
package proc

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

type RunOptions struct {
	Dir string
	// Env is the child's environment before launcher-only settings are removed.
	// nil means the current process environment.
	Env    launcherenv.Env
	OnLine func(line string)
	// OnStderr receives raw stderr bytes, for callers that parse private diagnostics.
	OnStderr  func(bytes []byte)
	MaxOutput int
	// StdoutOnly keeps structured stdout separate from stderr diagnostics; line callbacks still see both.
	StdoutOnly bool
	// Detached gives a game its own Linux process group; downloads retain their defaults.
	Detached bool
	// OutputDrain, when DrainAfterExit is set, bounds the wait for output after
	// the child exits but inherited pipes stay open.
	DrainAfterExit bool
	OutputDrain    time.Duration
}

type Result struct {
	Code   int
	Output string
}

type Tracked struct {
	Cmd   *exec.Cmd
	Stdin io.WriteCloser

	done     chan struct{}
	finished sync.Once
	exited   atomic.Bool
	result   Result

	callbacks sync.Mutex
	output    struct {
		sync.Mutex
		chunks []string
		length int
	}
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM", syscall.SIGKILL: "SIGKILL",
	syscall.SIGSEGV: "SIGSEGV", syscall.SIGABRT: "SIGABRT", syscall.SIGPIPE: "SIGPIPE",
}

// lookPath resolves command with the child's PATH, as Node's spawn does.
func lookPath(command string, env launcherenv.Env) (string, error) {
	if strings.Contains(command, "/") {
		return command, nil
	}
	for _, directory := range filepath.SplitList(env["PATH"]) {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, command)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &fs.PathError{Op: "exec", Path: command, Err: syscall.ENOENT}
}

// Spawn starts command without a shell. Wait returns the exit code (-1 when
// killed by a signal) and the bounded, ANSI-stripped output.
func Spawn(command string, args []string, options RunOptions) (*Tracked, error) {
	env := launcherenv.External(options.Env)
	observation := diagnostics.ObserveCommand(command, args)
	path, err := lookPath(command, env)
	if err != nil {
		observation.SpawnError(err)
		return nil, err
	}
	cmd := &exec.Cmd{Path: path, Args: append([]string{command}, args...), Env: env.List(), Dir: options.Dir}
	if options.Detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		outRead.Close()
		outWrite.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outWrite, errWrite
	if err := cmd.Start(); err != nil {
		for _, file := range []*os.File{outRead, outWrite, errRead, errWrite} {
			file.Close()
		}
		observation.SpawnError(err)
		return nil, err
	}
	outWrite.Close()
	errWrite.Close()

	t := &Tracked{Cmd: cmd, Stdin: stdin, done: make(chan struct{})}
	max := options.MaxOutput
	if max <= 0 {
		max = 2_000_000
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go t.read(outRead, true, max, options, nil, &readers)
	go t.read(errRead, false, max, options, observation, &readers)

	go func() {
		cmd.Wait()
		code, signal := -1, ""
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
			if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signal = signalNames[status.Signal()]
			}
		}
		t.exited.Store(true)
		if options.DrainAfterExit {
			// Only selected callers opt in. An inherited pipe is not evidence
			// that its original process is still alive; retain the real exit status.
			timer := time.AfterFunc(options.OutputDrain, func() {
				outRead.Close()
				errRead.Close()
			})
			readers.Wait()
			timer.Stop()
		} else {
			readers.Wait()
		}
		outRead.Close()
		errRead.Close()
		observation.Exit(code, signal)
		t.finish(code)
	}()
	return t, nil
}

func (t *Tracked) read(pipe *os.File, stdout bool, max int, options RunOptions, observation *diagnostics.Observation, done *sync.WaitGroup) {
	defer done.Done()
	var carry []byte
	buffered := ""
	chunk := make([]byte, 32*1024)
	for {
		n, err := pipe.Read(chunk)
		if n > 0 {
			raw := chunk[:n]
			if !stdout {
				observation.Stderr(raw)
				if options.OnStderr != nil {
					t.callbacks.Lock()
					options.OnStderr(append([]byte(nil), raw...))
					t.callbacks.Unlock()
				}
			}
			// Escape sequences and UTF-8 characters can straddle pipe reads.
			// Split complete terminal frames first, then remove ANSI.
			var text string
			text, carry = decodeUTF8(append(carry, raw...))
			buffered += text
			lines := splitTerminal(buffered)
			buffered = lines[len(lines)-1]
			t.emitLines(lines[:len(lines)-1], options.OnLine)
			if len(buffered) > 64_000 {
				buffered = tailString(buffered, 64_000)
			}
			if stdout || !options.StdoutOnly {
				t.appendOutput(shared.StripANSI(text), max)
			}
		}
		if err != nil {
			break
		}
	}
	if len(carry) > 0 {
		buffered += strings.ToValidUTF8(string(carry), "�")
	}
	if strings.TrimSpace(buffered) != "" {
		t.emitLines([]string{buffered}, options.OnLine)
	}
}

func (t *Tracked) emitLines(lines []string, onLine func(string)) {
	if onLine == nil {
		return
	}
	for _, raw := range lines {
		line := strings.TrimRightFunc(shared.StripANSI(raw), unicode.IsSpace)
		if line != "" {
			t.callbacks.Lock()
			onLine(line)
			t.callbacks.Unlock()
		}
	}
}

func (t *Tracked) appendOutput(clean string, max int) {
	t.output.Lock()
	defer t.output.Unlock()
	t.output.chunks = append(t.output.chunks, clean)
	t.output.length += len(clean)
	for t.output.length > max && len(t.output.chunks) > 1 {
		t.output.length -= len(t.output.chunks[0])
		t.output.chunks = t.output.chunks[1:]
	}
}

func (t *Tracked) finish(code int) {
	t.finished.Do(func() {
		t.output.Lock()
		t.result = Result{Code: code, Output: strings.Join(t.output.chunks, "")}
		t.output.Unlock()
		close(t.done)
	})
}

// Wait blocks until the child has exited and its output is drained.
func (t *Tracked) Wait() Result {
	<-t.done
	return t.result
}

// Done is closed when Wait would return.
func (t *Tracked) Done() <-chan struct{} { return t.done }

// Exited reports whether the child process itself has exited.
func (t *Tracked) Exited() bool { return t.exited.Load() }

func (t *Tracked) Pid() int {
	if t.Cmd.Process == nil {
		return 0
	}
	return t.Cmd.Process.Pid
}

// Signal sends sig to the child unless it has already exited.
func (t *Tracked) Signal(sig syscall.Signal) error {
	if t.Exited() {
		return nil
	}
	err := t.Cmd.Process.Signal(sig)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (t *Tracked) Kill() error { return t.Signal(syscall.SIGKILL) }

// Run spawns command and waits for it.
func Run(command string, args []string, options RunOptions) (Result, error) {
	tracked, err := Spawn(command, args, options)
	if err != nil {
		return Result{}, err
	}
	return tracked.Wait(), nil
}

// decodeUTF8 returns the valid prefix of data as text and any trailing bytes
// that may begin an incomplete UTF-8 sequence.
func decodeUTF8(data []byte) (string, []byte) {
	end := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-3; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				end = i
			}
			break
		}
	}
	return strings.ToValidUTF8(string(data[:end]), "�"), append([]byte(nil), data[end:]...)
}

// splitTerminal mirrors value.split(/[\r\n]/): the last element is the
// incomplete remainder.
func splitTerminal(value string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] == '\r' || value[i] == '\n' {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	return append(parts, value[start:])
}

func tailString(value string, n int) string {
	start := len(value) - n
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}
