package game

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/proc"
)

// StopPrefix is the generated Proton runner control; its environment targets one game prefix.
type StopPrefix struct {
	Command string
	Args    []string
}

type Process struct {
	*proc.Tracked
	options    proc.RunOptions
	stopPrefix *StopPrefix
	prefixOnce sync.Once
	prefixErr  error
}

// Spawn starts a game in its own process group.
func Spawn(command string, args []string, options proc.RunOptions, stopPrefix *StopPrefix) (*Process, error) {
	options.Detached = true
	// The managed runner waits for this prefix's Wine processes. Only then is
	// it safe to stop waiting for pipes inherited by non-Wine helpers. Legacy
	// direct-Wine launches retain their previous output/lifetime behavior.
	if stopPrefix != nil {
		options.DrainAfterExit, options.OutputDrain = true, time.Second
	}
	tracked, err := proc.Spawn(command, args, options)
	if err != nil {
		return nil, err
	}
	return &Process{Tracked: tracked, options: options, stopPrefix: stopPrefix}, nil
}

func (p *Process) signal(sig syscall.Signal) error {
	pid := p.Pid()
	if pid == 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (p *Process) groupAlive() bool {
	pid := p.Pid()
	return pid != 0 && syscall.Kill(-pid, 0) == nil
}

// Cleanup terminates the whole process group, escalating to SIGKILL after a second.
func (p *Process) Cleanup() error {
	if err := p.signal(syscall.SIGTERM); err != nil {
		return err
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if !p.groupAlive() {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return p.signal(syscall.SIGKILL)
}

// Stop asks the game to close.
func (p *Process) Stop() error { return p.signal(syscall.SIGINT) }

func (p *Process) runStopPrefix() error {
	p.prefixOnce.Do(func() {
		if p.stopPrefix == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.stopPrefix.Command, p.stopPrefix.Args...)
		cmd.Env = launcherenv.External(p.options.Env).List()
		if err := cmd.Run(); err != nil {
			p.prefixErr = fmt.Errorf("Could not stop the game's Wine prefix: %s", err.Error())
		}
	})
	return p.prefixErr
}

// ForceStop stops the managed game prefix (never the user's default/global
// wineserver, since Wine services detach from the launch process group) and
// then the process group.
func (p *Process) ForceStop() error {
	prefixErr := p.runStopPrefix()
	cleanupErr := p.Cleanup()
	return errors.Join(prefixErr, cleanupErr)
}
