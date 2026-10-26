package auth

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/launcherenv"
)

// View hosts Microsoft sign-in pages for one backend login.
type View interface {
	Open(frame Frame) error
	Close(session int64)
	Context(session int64, context string) error
	Dispose()
}

// Callbacks deliver page events back to the transport.
type Callbacks struct {
	Send   func(session int64, payload string)
	Cancel func(session int64)
	Fail   func()
}

type ViewFactory func(Callbacks) View

type Login struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
	err  error
}

// Wait returns the login exit code (1 when the user closed the window).
func (l *Login) Wait() (int, error) {
	<-l.done
	return l.code, l.err
}

func (l *Login) Kill() {
	if l.cmd.Process != nil {
		l.cmd.Process.Signal(syscall.SIGKILL)
	}
}

// Start runs `--external-webview-stdio login`. The flag is absent in the
// native backend: there is never a fallback to an incompatible handshake. The
// backend uses only these private stdio pipes.
func Start(binary string, createView ViewFactory, env launcherenv.Env, timeout time.Duration) (*Login, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	childEnv := launcherenv.External(env)
	childEnv["XODUS_LOG"], childEnv["RUST_LOG"] = "off", "off"
	cmd := exec.Command(binary, "--external-webview-stdio", "login")
	cmd.Env = childEnv.List()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	observation := diagnostics.ObserveChild("login")
	credentials := diagnostics.NewCredentialErrorReader()
	var stderrMu sync.Mutex
	cmd.Stderr = writer(func(b []byte) (int, error) {
		stderrMu.Lock()
		credentials.Push(b)
		stderrMu.Unlock()
		observation.Stderr(b)
		return len(b), nil
	})
	if err := cmd.Start(); err != nil {
		observation.SpawnError(err)
		return nil, errors.New("Unable to start the external sign-in backend.")
	}

	var (
		mu                        sync.Mutex
		ready, stopped, cancelled bool
		active                    int64 = -1
		latest                    int64
		failure                   error
		killTimer                 *time.Timer
	)
	terminate := func() {
		if stopped {
			return
		}
		cmd.Process.Signal(syscall.SIGTERM)
		if killTimer == nil {
			killTimer = time.AfterFunc(1500*time.Millisecond, func() { cmd.Process.Signal(syscall.SIGKILL) })
		}
	}
	fail := func(message string) {
		mu.Lock()
		defer mu.Unlock()
		if failure == nil {
			failure = errors.New(message)
			terminate()
		}
	}
	write := func(event any) {
		if stopped {
			return
		}
		data, _ := json.Marshal(event)
		if _, err := stdin.Write(append(data, '\n')); err != nil && !stopped && !cancelled {
			failure = errors.New("The sign-in backend disconnected.")
			terminate()
		}
	}
	view := createView(Callbacks{
		Send: func(session int64, payload string) {
			mu.Lock()
			defer mu.Unlock()
			if active == session && len(payload) <= FrameLimit/2 {
				write(map[string]any{"type": "ipc", "session": session, "payload": payload})
			}
		},
		Cancel: func(session int64) {
			mu.Lock()
			defer mu.Unlock()
			if active != session || stopped {
				return
			}
			cancelled = true
			write(map[string]any{"type": "closed", "session": session})
			terminate()
		},
		Fail: func() { fail("The sign-in window was blocked or failed to load.") },
	})
	timer := time.AfterFunc(timeout, func() { fail("Sign-in timed out.") })
	force := time.AfterFunc(timeout+3*time.Second, func() { cmd.Process.Signal(syscall.SIGKILL) })

	frames := make(chan string, 64)
	var leftover bool
	var readErr error
	go func() {
		defer close(frames)
		reader := bufio.NewReaderSize(io.LimitReader(stdout, 2*1024*1024+1), FrameLimit+2)
		total := 0
		for {
			line, err := reader.ReadSlice('\n')
			total += len(line)
			if total > 2*1024*1024 {
				fail("Sign-in backend exceeded the output limit.")
				return
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				fail("Sign-in response exceeds the protocol limit.")
				return
			}
			if len(line) > 0 && line[len(line)-1] == '\n' {
				frames <- strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			} else if len(line) > 0 {
				leftover = true
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr = err
				}
				return
			}
		}
	}()
	processed := make(chan struct{})
	completing := false
	// Page loads run asynchronously so later frames, including the backend
	// completing the session, are processed while a view is loading.
	viewFailed := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		done := completing
		mu.Unlock()
		if !done {
			fail("The sign-in window was blocked or failed to load.")
		}
	}
	go func() {
		defer close(processed)
		for raw := range frames {
			mu.Lock()
			skip := failure != nil || cancelled
			mu.Unlock()
			if skip {
				continue
			}
			if err := handle(raw, &mu, &ready, &active, &latest, &stopped, write, view, viewFailed); err != nil {
				fail("The sign-in backend returned an invalid response.")
			}
		}
	}()

	login := &Login{cmd: cmd, done: make(chan struct{})}
	go func() {
		<-processed
		cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		observation.Exit(code, "")
		mu.Lock()
		// Disposing an already successful sign-in can abort page loads. Ignore
		// only those view errors, never protocol validation failures.
		completing = code == 0 && !cancelled && failure == nil
		stopped = true
		timer.Stop()
		force.Stop()
		if killTimer != nil {
			killTimer.Stop()
		}
		mu.Unlock()
		view.Dispose()
		stdin.Close()
		mu.Lock()
		defer mu.Unlock()
		stderrMu.Lock()
		message := credentials.Message
		stderrMu.Unlock()
		switch {
		case failure != nil:
			diagnostics.Emit(diagnostics.Record{Operation: "login", Event: "validation", Stage: "protocol-error"})
			login.err = failure
		case code != 0 && message != "":
			login.err = errors.New(message)
		case !ready || leftover || readErr != nil || (code == 0 && (active != -1 || latest == 0)):
			diagnostics.Emit(diagnostics.Record{Operation: "login", Event: "validation", Stage: "protocol-error"})
			login.err = errors.New("The sign-in backend did not complete its protocol.")
		default:
			login.code = code
			if cancelled {
				login.code = 1
			}
		}
		close(login.done)
	}()
	return login, nil
}

func handle(raw string, mu *sync.Mutex, ready *bool, active, latest *int64, stopped *bool, write func(any), view View, viewFailed func(error)) error {
	frame, err := ParseFrame(raw)
	if err != nil || frame == nil {
		return err
	}
	mu.Lock()
	if frame.Type == Ready {
		defer mu.Unlock()
		if *ready {
			return errors.New("Duplicate sign-in handshake.")
		}
		*ready = true
		write(map[string]any{"type": "hello", "version": 1})
		return nil
	}
	if !*ready {
		mu.Unlock()
		return errors.New("Missing sign-in protocol handshake.")
	}
	switch {
	case frame.Type == Open:
		if frame.Session <= *latest || *latest >= 32 {
			mu.Unlock()
			return errors.New("Invalid sign-in session order.")
		}
		*active, *latest = frame.Session, frame.Session
		isStopped := *stopped
		mu.Unlock()
		if !isStopped {
			go func() { viewFailed(view.Open(*frame)) }()
		}
	case frame.Session == *active:
		if frame.Type == Context {
			isStopped := *stopped
			mu.Unlock()
			if !isStopped {
				go func() { viewFailed(view.Context(frame.Session, frame.Context)) }()
			}
		} else {
			*active = -1
			mu.Unlock()
			view.Close(frame.Session)
		}
	default:
		mu.Unlock()
		return errors.New("Unexpected sign-in session.")
	}
	return nil
}

type writer func([]byte) (int, error)

func (w writer) Write(b []byte) (int, error) { return w(b) }
