package account

import (
	"context"
	"sync"
	"time"

	"xodus-nexus-wails/internal/proc"
)

// ReadAccount reads account status, never an interactive sign-in or token renewal.
func ReadAccount(ctx context.Context, binary string) (proc.Result, error) {
	if err := ctx.Err(); err != nil {
		return proc.Result{}, err
	}
	tracked, err := proc.Spawn(binary, []string{"account", "--json"}, proc.RunOptions{MaxOutput: 20_000, StdoutOnly: true})
	if err != nil {
		return proc.Result{}, err
	}
	timer := time.AfterFunc(15*time.Second, func() { tracked.Kill() })
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { tracked.Kill() })
	defer stop()
	return tracked.Wait(), nil
}

type pendingRefresh struct {
	key    string
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

// Refresh shares same-identity reads and prevents obsolete reads from publishing state.
type Refresh struct {
	mu         sync.Mutex
	generation int
	pending    *pendingRefresh
}

func (r *Refresh) invalidateLocked() {
	r.generation++
	if r.pending != nil {
		r.pending.cancel()
	}
	r.pending = nil
}

func (r *Refresh) Invalidate() {
	r.mu.Lock()
	r.invalidateLocked()
	r.mu.Unlock()
}

// Run reads with read and publishes the value unless a newer run or current()
// says the result is obsolete. Errors of obsolete runs are dropped.
func Run[T any](r *Refresh, key string, read func(context.Context) (T, error), publish func(T) error, current func() bool) error {
	r.mu.Lock()
	if r.pending != nil && r.pending.key == key {
		pending := r.pending
		r.mu.Unlock()
		<-pending.done
		return pending.err
	}
	r.invalidateLocked()
	r.generation++
	generation := r.generation
	ctx, cancel := context.WithCancel(context.Background())
	pending := &pendingRefresh{key: key, done: make(chan struct{}), cancel: cancel}
	r.pending = pending
	r.mu.Unlock()

	live := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return generation == r.generation && current()
	}
	value, err := read(ctx)
	switch {
	case err == nil && ctx.Err() == nil && live():
		err = publish(value)
	case err != nil && live():
	default:
		err = nil
	}
	pending.err = err
	r.mu.Lock()
	if r.pending == pending {
		r.pending = nil
	}
	r.mu.Unlock()
	close(pending.done)
	return err
}
