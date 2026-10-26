// Package install ports install-queue.ts, download-cleanup.ts, install-plan.ts,
// game-removal.ts, game-delete.ts and the small download helpers.
package install

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/shared"
)

type Job struct {
	ID     string
	Run    func() error
	Failed func(error) error
}

// Queue runs one installation at a time. A paused live process retains its
// slot. Only process cleanup releases it.
type Queue struct {
	idle    func()
	mu      sync.Mutex
	jobs    []Job
	active  string
	stopped bool
	holds   int
	waiters map[string][]chan struct{}
}

func NewQueue(idle func()) *Queue {
	if idle == nil {
		idle = func() {}
	}
	return &Queue{idle: idle, waiters: map[string][]chan struct{}{}}
}

func (q *Queue) Size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.jobs)
	if q.active != "" {
		n++
	}
	return n
}

func (q *Queue) hasLocked(id string) bool {
	return q.active == id || slices.ContainsFunc(q.jobs, func(job Job) bool { return job.ID == id })
}

func (q *Queue) Has(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.hasLocked(id)
}

// Hold pauses starting new jobs until the returned release is called.
func (q *Queue) Hold() (release func()) {
	q.mu.Lock()
	q.holds++
	q.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			q.mu.Lock()
			q.holds--
			q.mu.Unlock()
			q.pump()
		})
	}
}

// Settled waits until id is neither queued nor running.
func (q *Queue) Settled(id string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	q.mu.Lock()
	if !q.hasLocked(id) {
		q.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	q.waiters[id] = append(q.waiters[id], done)
	q.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		q.mu.Lock()
		q.waiters[id] = slices.DeleteFunc(q.waiters[id], func(c chan struct{}) bool { return c == done })
		if len(q.waiters[id]) == 0 {
			delete(q.waiters, id)
		}
		q.mu.Unlock()
		return errors.New("The download is still stopping. Files were not deleted.")
	}
}

func (q *Queue) notifyLocked(id string) {
	for _, waiter := range q.waiters[id] {
		close(waiter)
	}
	delete(q.waiters, id)
}

func (q *Queue) Enqueue(job Job) error {
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return errors.New("The launcher is closing.")
	}
	if q.hasLocked(job.ID) {
		q.mu.Unlock()
		return errors.New("Installation is already queued.")
	}
	q.jobs = append(q.jobs, job)
	q.mu.Unlock()
	q.pump()
	return nil
}

func (q *Queue) Cancel(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	index := slices.IndexFunc(q.jobs, func(job Job) bool { return job.ID == id })
	if index < 0 {
		return false
	}
	q.jobs = slices.Delete(q.jobs, index, index+1)
	q.notifyLocked(id)
	return true
}

func (q *Queue) Stop() {
	q.mu.Lock()
	q.stopped = true
	q.mu.Unlock()
}

func (q *Queue) pump() {
	q.mu.Lock()
	if q.active != "" || q.stopped || q.holds > 0 || len(q.jobs) == 0 {
		q.mu.Unlock()
		return
	}
	job := q.jobs[0]
	q.jobs = q.jobs[1:]
	q.active = job.ID
	q.mu.Unlock()
	go func() {
		if err := job.Run(); err != nil {
			if failedErr := job.Failed(err); failedErr != nil {
				q.mu.Lock()
				q.stopped = true
				q.mu.Unlock()
				log.Print("Could not persist installation failure; queue stopped.")
			}
		}
		q.mu.Lock()
		q.active = ""
		q.notifyLocked(job.ID)
		empty := len(q.jobs) == 0
		q.mu.Unlock()
		if empty {
			q.idle()
		}
		q.pump()
	}()
}

// AssertActivityCompatible refuses to start mode while another Xodus operation
// is active. mode is "", "install", "launch" or "remove".
func AssertActivityCompatible(operations []string, downloads []shared.DownloadTask, game *shared.GameOperation, queued int, mode string) error {
	downloadIDs := map[string]bool{}
	for _, task := range downloads {
		downloadIDs[task.ID] = true
	}
	gameActive := game != nil && (game.Status == "preparing" || game.Status == "running" || game.Status == "stopping")
	allowedGame := (mode == "install" || mode == "remove") && gameActive && game.Kind == "launch" && game.Status == "running"
	blocked := slices.ContainsFunc(operations, func(id string) bool {
		return !(mode != "" && downloadIDs[id]) && !(allowedGame && game.ID == id)
	})
	if (mode == "" && queued > 0) || (gameActive && !allowedGame) || blocked {
		return errors.New("Another Xodus operation is active. Wait for it to finish or stop it first.")
	}
	return nil
}

// CanonicalInstallPath resolves existing ancestors too, so a new directory
// through a symlink is locked.
func CanonicalInstallPath(input string) (string, error) {
	if !filepath.IsAbs(input) || strings.ContainsRune(input, 0) {
		return "", errors.New("The destination must be an absolute path.")
	}
	resolved := filepath.Clean(input)
	real, err := filepath.EvalSymlinks(resolved)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// EvalSymlinks cannot resolve a dangling link, but its future destination
	// still reserves real files under the target. Do not treat the link as a new folder.
	if info, lerr := os.Lstat(resolved); lerr == nil && info.Mode()&fs.ModeSymlink != 0 {
		target, rerr := os.Readlink(resolved)
		if rerr != nil {
			return "", rerr
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(resolved), target)
		}
		return CanonicalInstallPath(filepath.Clean(target))
	} else if lerr != nil && !errors.Is(lerr, fs.ErrNotExist) {
		return "", lerr
	}
	parent := filepath.Dir(resolved)
	if parent == resolved {
		return "", err
	}
	canonicalParent, perr := CanonicalInstallPath(parent)
	if perr != nil {
		return "", perr
	}
	return filepath.Join(canonicalParent, filepath.Base(resolved)), nil
}

func contains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)))
}

func PathsOverlap(left, right string) bool { return contains(left, right) || contains(right, left) }

func AssertInstallDestination(destination string, locked []string) (string, error) {
	canonical, err := CanonicalInstallPath(destination)
	if err != nil {
		return "", err
	}
	for _, item := range locked {
		other, err := CanonicalInstallPath(item)
		if err != nil {
			return "", err
		}
		if PathsOverlap(canonical, other) {
			return "", errors.New("This location overlaps an installed game or another installation. Choose a separate folder.")
		}
	}
	return canonical, nil
}
