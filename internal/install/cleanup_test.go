package install

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"xodus-nexus-wails/internal/shared"
)

type cleanupFixture struct {
	root, state string
	task        shared.DownloadTask
	cleanup     *Cleanup
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(root, "profile")
	return &cleanupFixture{root: root, state: state, cleanup: NewCleanup(state, nil, nil),
		task: shared.DownloadTask{ID: NewUUID(), Kind: "install", ProductID: "SAMPLE", Title: "Fixture", Destination: filepath.Join(root, "new-install"), Status: "running", Log: []string{}}}
}

func (f *cleanupFixture) available() bool {
	return f.cleanup.Options(f.task, nil, []string{f.state}).DeleteFilesAllowed
}

func (f *cleanupFixture) partial(t *testing.T) {
	if err := f.cleanup.Prepare(f.task); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(f.task.Destination, "partial.bin"), []byte("disposable download"), 0o644)
}

func (f *cleanupFixture) partialIntact(t *testing.T, directory string) {
	t.Helper()
	if data, _ := os.ReadFile(filepath.Join(directory, "partial.bin")); string(data) != "disposable download" {
		t.Error("partial download was touched")
	}
}

func TestCleanupDeletesOnlyProvenFolder(t *testing.T) {
	f := newCleanupFixture(t)
	game := filepath.Join(f.root, "existing-game")
	os.Mkdir(game, 0o755)
	os.WriteFile(filepath.Join(game, "save"), []byte("keep"), 0o644)
	f.partial(t)
	if !f.available() {
		t.Fatal("new tracked folder should be deletable")
	}
	if err := f.cleanup.Remove(f.task, []string{game}, []string{f.state}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.task.Destination); err == nil {
		t.Error("folder still exists")
	}
	if data, _ := os.ReadFile(filepath.Join(game, "save")); string(data) != "keep" {
		t.Error("neighbouring game touched")
	}
}

func TestCleanupNeverClaimsExistingFolder(t *testing.T) {
	for _, populated := range []bool{false, true} {
		f := newCleanupFixture(t)
		os.Mkdir(f.task.Destination, 0o755)
		if populated {
			os.WriteFile(filepath.Join(f.task.Destination, "original"), []byte("keep"), 0o644)
		}
		f.cleanup.Prepare(f.task)
		if f.available() {
			t.Error("pre-existing folder claimed")
		}
		if err := f.cleanup.Remove(f.task, nil, nil); err == nil || !strings.Contains(err.Error(), "Existing or untracked") {
			t.Errorf("remove: %v", err)
		}
		entries, _ := os.ReadDir(f.task.Destination)
		if populated != (len(entries) == 1) {
			t.Errorf("entries = %v", entries)
		}
	}
}

func TestCleanupRefusesUpdatesAndMismatches(t *testing.T) {
	for _, kind := range []string{"update", "content", ""} {
		f := newCleanupFixture(t)
		f.partial(t)
		f.task.Kind = kind
		if f.available() {
			t.Errorf("%q deletable", kind)
		}
		if err := f.cleanup.Remove(f.task, nil, nil); err == nil || !strings.Contains(err.Error(), "Updates and DLC") {
			t.Errorf("%q: %v", kind, err)
		}
		f.partialIntact(t, f.task.Destination)
	}
	f := newCleanupFixture(t)
	f.partial(t)
	for i, change := range []func(*shared.DownloadTask){
		func(t *shared.DownloadTask) { t.GameID = "existing" }, func(t *shared.DownloadTask) { t.Status = "complete" },
		func(t *shared.DownloadTask) { t.ID = NewUUID() }, func(t *shared.DownloadTask) { t.ProductID = "OTHER" },
		func(t *shared.DownloadTask) { t.Destination = f.root },
	} {
		task := f.task
		change(&task)
		if f.cleanup.Options(task, nil, nil).DeleteFilesAllowed {
			t.Errorf("mismatch %d allowed", i)
		}
	}
}

func TestCleanupProofPersistsAndCompletes(t *testing.T) {
	f := newCleanupFixture(t)
	f.partial(t)
	f.cleanup = NewCleanup(f.state, nil, nil)
	if !f.available() {
		t.Fatal("proof must survive restarts")
	}
	f.cleanup.Complete(f.task)
	if f.available() {
		t.Error("completed task still deletable")
	}
	if entries, _ := os.ReadDir(f.task.Destination); len(entries) != 1 || entries[0].Name() != "partial.bin" {
		t.Errorf("entries = %v", entries)
	}
}

func TestCleanupResumeInheritance(t *testing.T) {
	f := newCleanupFixture(t)
	f.partial(t)
	f.task.Status, f.task.SelectedContentID, f.task.SelectionMarket = "paused", NewUUID(), "US"
	resumed := f.task
	resumed.ID, resumed.Status = NewUUID(), "queued"
	f.cleanup.InheritResume(f.task, resumed)
	f.cleanup.Prepare(resumed)
	if !f.cleanup.Options(resumed, nil, []string{f.state}).DeleteFilesAllowed {
		t.Fatal("resumed task should inherit ownership")
	}
	if err := f.cleanup.Remove(resumed, nil, []string{f.state}); err != nil {
		t.Fatal(err)
	}

	g := newCleanupFixture(t)
	g.partial(t)
	g.task.Status, g.task.SelectedContentID, g.task.SelectionMarket = "paused", NewUUID(), "US"
	for i, change := range []func(*shared.DownloadTask){
		func(t *shared.DownloadTask) { t.ProductID = "DIFFERENT" }, func(t *shared.DownloadTask) { t.SelectedContentID = NewUUID() },
		func(t *shared.DownloadTask) { t.SelectionMarket = "DE" }, func(t *shared.DownloadTask) { t.Destination = filepath.Join(g.root, "other") },
		func(t *shared.DownloadTask) { t.Kind = "update" }, func(t *shared.DownloadTask) { t.Kind = "content" },
		func(t *shared.DownloadTask) { t.GameID = "existing" },
	} {
		next := g.task
		next.ID, next.Status = NewUUID(), "queued"
		change(&next)
		g.cleanup.InheritResume(g.task, next)
		if g.cleanup.Options(next, nil, []string{g.state}).DeleteFilesAllowed {
			t.Errorf("change %d inherited ownership", i)
		}
	}
	if !g.available() {
		t.Error("original ownership lost")
	}

	original := filepath.Join(g.root, "old-owned")
	os.Rename(g.task.Destination, original)
	os.Mkdir(g.task.Destination, 0o755)
	replaced := g.task
	replaced.ID, replaced.Status = NewUUID(), "queued"
	g.cleanup.InheritResume(g.task, replaced)
	if g.cleanup.Options(replaced, nil, []string{g.state}).DeleteFilesAllowed {
		t.Error("replaced directory inherited ownership")
	}
}

func TestCleanupRejectsReplacements(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink", "file", "missing"} {
		f := newCleanupFixture(t)
		f.partial(t)
		original := filepath.Join(f.root, "original")
		os.Rename(f.task.Destination, original)
		switch replacement {
		case "directory":
			os.Mkdir(f.task.Destination, 0o755)
		case "symlink":
			os.Symlink(original, f.task.Destination)
		case "file":
			os.WriteFile(f.task.Destination, []byte("keep"), 0o644)
		}
		if err := f.cleanup.Remove(f.task, nil, nil); err == nil {
			t.Errorf("%s replacement removed", replacement)
		}
		f.partialIntact(t, original)
	}
}

func TestCleanupRejectsOverlapsAndLinks(t *testing.T) {
	f := newCleanupFixture(t)
	f.partial(t)
	alias := filepath.Join(f.root, "alias")
	os.Symlink(f.task.Destination, alias)
	for _, locked := range []string{f.root, f.task.Destination, alias, filepath.Join(f.task.Destination, "nested")} {
		if f.cleanup.Options(f.task, []string{locked}, nil).DeleteFilesAllowed {
			t.Errorf("locked %s ignored", locked)
		}
	}
	if f.cleanup.Options(f.task, nil, []string{f.task.Destination}).DeleteFilesAllowed {
		t.Error("protected root ignored")
	}

	for _, mode := range []string{"changed", "missing", "symlink"} {
		g := newCleanupFixture(t)
		g.partial(t)
		entries, _ := os.ReadDir(g.task.Destination)
		index := slices.IndexFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), ".nexus-download-") })
		marker := filepath.Join(g.task.Destination, entries[index].Name())
		os.Remove(marker)
		switch mode {
		case "changed":
			os.WriteFile(marker, []byte("wrong"), 0o600)
		case "symlink":
			os.Symlink(filepath.Join(g.task.Destination, "partial.bin"), marker)
		}
		if err := g.cleanup.Remove(g.task, nil, nil); err == nil {
			t.Errorf("%s marker trusted", mode)
		}
		g.partialIntact(t, g.task.Destination)
	}

	h := newCleanupFixture(t)
	h.partial(t)
	record := filepath.Join(h.state, "download-ownership", h.task.ID+".json")
	outside := filepath.Join(h.root, "proof-copy")
	os.Rename(record, outside)
	os.Symlink(outside, record)
	if h.available() {
		t.Error("symlinked proof trusted")
	}
	os.Remove(record)
	os.Rename(outside, record)
	directory := filepath.Join(h.state, "download-ownership")
	os.Rename(directory, directory+"-moved")
	os.Symlink(directory+"-moved", directory)
	if h.available() {
		t.Error("symlinked ownership directory trusted")
	}
}

func TestCleanupPreservesUnsafeTrees(t *testing.T) {
	f := newCleanupFixture(t)
	f.partial(t)
	outside := filepath.Join(f.root, "outside-save")
	os.WriteFile(outside, []byte("keep"), 0o644)
	os.Symlink(outside, filepath.Join(f.task.Destination, "link"))
	if err := f.cleanup.Remove(f.task, nil, nil); err == nil || !strings.Contains(err.Error(), "Files were not deleted") {
		t.Errorf("unsafe link: %v", err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep" {
		t.Error("linked save touched")
	}
	f.partialIntact(t, f.task.Destination)

	g := newCleanupFixture(t)
	g.partial(t)
	removed := false
	g.cleanup = NewCleanup(g.state, func(string) error { removed = true; return nil }, func() (string, error) {
		entries, _ := os.ReadDir(g.root)
		index := slices.IndexFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), ".nexus-cancel-") })
		return "1 2 0:1 / " + filepath.Join(g.root, entries[index].Name(), "files", "bind") + " rw - tmpfs tmpfs rw", nil
	})
	if err := g.cleanup.Remove(g.task, nil, nil); err == nil || !strings.Contains(err.Error(), "mounted filesystem") || removed {
		t.Errorf("bind mount: %v removed=%v", err, removed)
	}
	g.partialIntact(t, g.task.Destination)
}

func TestCleanupPartialFailureRecovery(t *testing.T) {
	f := newCleanupFixture(t)
	f.partial(t)
	os.WriteFile(filepath.Join(f.task.Destination, "remaining"), []byte("keep"), 0o644)
	f.cleanup = NewCleanup(f.state, func(directory string) error {
		os.Remove(filepath.Join(directory, "partial.bin"))
		return errors.New("Synthetic disk failure")
	}, nil)
	if err := f.cleanup.Remove(f.task, nil, nil); err == nil || !strings.Contains(err.Error(), "some downloaded files may already have been deleted") {
		t.Errorf("partial failure: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(f.task.Destination, "remaining")); string(data) != "keep" {
		t.Error("remaining files not restored")
	}

	g := newCleanupFixture(t)
	g.partial(t)
	g.cleanup = NewCleanup(g.state, func(string) error {
		os.Mkdir(g.task.Destination, 0o755)
		os.WriteFile(filepath.Join(g.task.Destination, "existing-game"), []byte("keep"), 0o644)
		return errors.New("Synthetic failure")
	}, nil)
	if err := g.cleanup.Remove(g.task, nil, nil); err == nil || !strings.Contains(err.Error(), "Remaining files are preserved at") {
		t.Errorf("replacement: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(g.task.Destination, "existing-game")); string(data) != "keep" {
		t.Error("replacement overwritten")
	}
	entries, _ := os.ReadDir(g.root)
	index := slices.IndexFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), ".nexus-cancel-") })
	g.partialIntact(t, filepath.Join(g.root, entries[index].Name(), "files"))
}
