package install

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xodus-nexus-wails/internal/shared"
)

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func noFailure(error) error { return nil }

type calls struct {
	sync.Mutex
	list []string
}

func (c *calls) add(name string) { c.Lock(); c.list = append(c.list, name); c.Unlock() }
func (c *calls) get() []string   { c.Lock(); defer c.Unlock(); return slices.Clone(c.list) }

func TestQueueHoldAndSettle(t *testing.T) {
	queue := NewQueue(nil)
	process, cleanup := make(chan struct{}), make(chan struct{})
	var next atomic.Int32
	queue.Enqueue(Job{ID: "active", Run: func() error { <-process; <-cleanup; return nil }, Failed: noFailure})
	queue.Enqueue(Job{ID: "next", Run: func() error { next.Add(1); return nil }, Failed: noFailure})
	release := queue.Hold()
	var settled atomic.Bool
	go func() { queue.Settled("active", 0); settled.Store(true) }()
	close(process)
	time.Sleep(10 * time.Millisecond)
	if settled.Load() {
		t.Fatal("settled before cleanup")
	}
	close(cleanup)
	eventually(t, settled.Load)
	time.Sleep(10 * time.Millisecond)
	if next.Load() != 0 {
		t.Fatal("held queue started the next job")
	}
	release()
	release()
	eventually(t, func() bool { return next.Load() == 1 })
}

func TestQueueCancelAndTimeout(t *testing.T) {
	queue := NewQueue(nil)
	process := make(chan struct{})
	var never atomic.Int32
	queue.Enqueue(Job{ID: "active", Run: func() error { <-process; return nil }, Failed: noFailure})
	queue.Enqueue(Job{ID: "queued", Run: func() error { never.Add(1); return nil }, Failed: noFailure})
	done := make(chan error)
	go func() { done <- queue.Settled("queued", 0) }()
	time.Sleep(5 * time.Millisecond)
	queue.Cancel("queued")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := queue.Settled("active", 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "Files were not deleted") || !queue.Has("active") {
		t.Errorf("timeout: %v", err)
	}
	if err := queue.Enqueue(Job{ID: "active", Run: func() error { return nil }, Failed: noFailure}); err == nil || !strings.Contains(err.Error(), "already queued") {
		t.Errorf("duplicate: %v", err)
	}
	if queue.Cancel("active") {
		t.Error("an active job cannot be cancelled from the queue")
	}
	close(process)
	queue.Settled("active", 0)
	if never.Load() != 0 {
		t.Error("cancelled job ran")
	}
}

func TestQueueOrderAndFailures(t *testing.T) {
	queue := NewQueue(nil)
	first, cleanup := make(chan struct{}), make(chan struct{})
	var order calls
	queue.Enqueue(Job{ID: "first", Run: func() error { order.add("first"); <-first; <-cleanup; return nil }, Failed: noFailure})
	queue.Enqueue(Job{ID: "second", Run: func() error { order.add("second"); return nil }, Failed: noFailure})
	queue.Enqueue(Job{ID: "third", Run: func() error { order.add("third"); return nil }, Failed: noFailure})
	eventually(t, func() bool { return len(order.get()) == 1 })
	if queue.Size() != 3 {
		t.Errorf("size = %d", queue.Size())
	}
	close(first)
	close(cleanup)
	eventually(t, func() bool { return queue.Size() == 0 })
	if !slices.Equal(order.get(), []string{"first", "second", "third"}) {
		t.Errorf("order = %v", order.get())
	}

	saved := make(chan struct{})
	var events calls
	failing := NewQueue(nil)
	failing.Enqueue(Job{ID: "bad", Run: func() error { return errors.New("offline") }, Failed: func(error) error { events.add("failure"); <-saved; return nil }})
	failing.Enqueue(Job{ID: "good", Run: func() error { events.add("good"); return nil }, Failed: noFailure})
	eventually(t, func() bool { return slices.Equal(events.get(), []string{"failure"}) })
	close(saved)
	eventually(t, func() bool { return slices.Equal(events.get(), []string{"failure", "good"}) })

	var never atomic.Int32
	broken := NewQueue(nil)
	broken.Enqueue(Job{ID: "bad", Run: func() error { return errors.New("bad") }, Failed: func(error) error { return errors.New("disk full") }})
	broken.Enqueue(Job{ID: "next", Run: func() error { never.Add(1); return nil }, Failed: noFailure})
	eventually(t, func() bool { return broken.Size() == 1 })
	time.Sleep(10 * time.Millisecond)
	if never.Load() != 0 {
		t.Error("queue must stop when a failure cannot be persisted")
	}
}

func TestQueueStop(t *testing.T) {
	queue := NewQueue(nil)
	hold := make(chan struct{})
	var never atomic.Int32
	queue.Enqueue(Job{ID: "active", Run: func() error { <-hold; return nil }, Failed: noFailure})
	queue.Enqueue(Job{ID: "queued", Run: func() error { never.Add(1); return nil }, Failed: noFailure})
	queue.Stop()
	close(hold)
	eventually(t, func() bool { return queue.Size() == 1 })
	if never.Load() != 0 {
		t.Error("queued job started after shutdown")
	}
	if err := queue.Enqueue(Job{ID: "new", Run: func() error { return nil }, Failed: noFailure}); err == nil || !strings.Contains(err.Error(), "closing") {
		t.Errorf("enqueue after stop: %v", err)
	}
}

func TestActivityGuards(t *testing.T) {
	downloads := []shared.DownloadTask{{ID: "download", Status: "running"}}
	game := &shared.GameOperation{ID: "game", Kind: "launch", Status: "running"}
	ok := func(operations []string, downloads []shared.DownloadTask, game *shared.GameOperation, queued int, mode string) bool {
		return AssertActivityCompatible(operations, downloads, game, queued, mode) == nil
	}
	if !ok([]string{"download", "game"}, downloads, game, 3, "install") || !ok([]string{"download", "game"}, downloads, game, 3, "remove") ||
		!ok([]string{"download"}, downloads, nil, 2, "remove") || !ok([]string{"download"}, downloads, nil, 2, "launch") {
		t.Error("compatible activity rejected")
	}
	if ok([]string{"download", "game"}, downloads, game, 2, "") || ok(nil, nil, nil, 1, "") || ok([]string{"game"}, nil, game, 0, "launch") {
		t.Error("exclusive activity allowed")
	}
	for _, mode := range []string{"launch", "install", "remove"} {
		if ok([]string{"auth"}, nil, nil, 0, mode) {
			t.Errorf("sign-in allowed during %s", mode)
		}
	}
	for _, mode := range []string{"install", "remove"} {
		for _, status := range []string{"preparing", "stopping"} {
			if ok([]string{"game"}, nil, &shared.GameOperation{ID: "game", Kind: "launch", Status: status}, 0, mode) {
				t.Errorf("%s allowed while game is %s", mode, status)
			}
		}
		if ok([]string{"game"}, nil, &shared.GameOperation{ID: "game", Kind: "prerequisite", Status: "running"}, 0, mode) {
			t.Errorf("%s allowed during prerequisite", mode)
		}
	}
}

func TestDestinationLocks(t *testing.T) {
	for _, other := range []string{"/games/title", "/games/title/dlc", "/games", "/"} {
		if !PathsOverlap("/games/title", other) {
			t.Errorf("%s should overlap", other)
		}
	}
	if PathsOverlap("/games/title", "/games/title-two") {
		t.Error("sibling prefix is not an overlap")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	os.Mkdir(filepath.Join(root, "game"), 0o755)
	os.Symlink(filepath.Join(root, "game"), filepath.Join(root, "alias"))
	if got, _ := CanonicalInstallPath(filepath.Join(root, "alias", "new")); got != filepath.Join(root, "game", "new") {
		t.Errorf("canonical = %q", got)
	}
	if _, err := AssertInstallDestination(filepath.Join(root, "alias", "new"), []string{filepath.Join(root, "game")}); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("alias: %v", err)
	}
	if got, err := AssertInstallDestination(filepath.Join(root, "other"), []string{filepath.Join(root, "game")}); err != nil || got != filepath.Join(root, "other") {
		t.Errorf("separate = %q, %v", got, err)
	}
	if _, err := CanonicalInstallPath("relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative: %v", err)
	}
	os.Symlink("game/future", filepath.Join(root, "dangling"))
	os.Symlink("dangling", filepath.Join(root, "chained"))
	if got, _ := CanonicalInstallPath(filepath.Join(root, "chained", "content")); got != filepath.Join(root, "game/future/content") {
		t.Errorf("chained = %q", got)
	}
	if _, err := AssertInstallDestination(filepath.Join(root, "dangling", "content"), []string{filepath.Join(root, "game")}); err == nil {
		t.Error("dangling link overlap")
	}
	os.Symlink("loop", filepath.Join(root, "loop"))
	if _, err := CanonicalInstallPath(filepath.Join(root, "loop", "content")); err == nil {
		t.Error("symlink loop accepted")
	}
}
