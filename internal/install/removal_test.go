package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xodus-nexus-wails/internal/shared"
)

func gameFixture(t *testing.T) shared.Game {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	directory := filepath.Join(parent, "Game with spaces; literal")
	os.Mkdir(directory, 0o755)
	os.WriteFile(filepath.Join(directory, "MicrosoftGame.config"), []byte("<Game/>"), 0o644)
	os.WriteFile(filepath.Join(directory, "data.bin"), []byte("fixture"), 0o644)
	return shared.Game{ID: "game", Title: "Test", Path: directory, InstallState: "ready"}
}

func download(destination, status string) shared.DownloadTask {
	return shared.DownloadTask{ID: "download", Title: "Synthetic installation", ProductID: rootID, Destination: destination, Status: status, Log: []string{}}
}

func inactive(shared.DownloadTask) bool { return false }

func TestDeleteTargetValidation(t *testing.T) {
	game := gameFixture(t)
	home, _ := os.UserHomeDir()
	if target, err := ValidateGameDeleteTarget(game, nil, []string{home}); err != nil || target.Path != game.Path {
		t.Errorf("isolated = %+v, %v", target, err)
	}
	nested := game
	nested.ID, nested.Path = "nested", filepath.Join(game.Path, "nested")
	if _, err := ValidateGameDeleteTarget(game, []shared.Game{nested}, nil); err == nil || !strings.Contains(err.Error(), "another library game") {
		t.Errorf("nested: %v", err)
	}
	parent := game
	parent.ID, parent.Path = "parent", filepath.Dir(game.Path)
	if _, err := ValidateGameDeleteTarget(game, []shared.Game{parent}, nil); err == nil || !strings.Contains(err.Error(), "another library game") {
		t.Errorf("parent: %v", err)
	}
	alias := filepath.Join(filepath.Dir(game.Path), "alias")
	os.Symlink(game.Path, alias)
	aliasGame := game
	aliasGame.ID, aliasGame.Path = "alias", alias
	if _, err := ValidateGameDeleteTarget(game, []shared.Game{aliasGame}, nil); err == nil || !strings.Contains(err.Error(), "another library game") {
		t.Errorf("alias game: %v", err)
	}
	if _, err := ValidateGameDeleteTarget(game, nil, []string{alias}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("protected alias: %v", err)
	}
	if _, err := ValidateGameDeleteTarget(game, nil, []string{filepath.Join(game.Path, "runtime")}); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Errorf("protected child: %v", err)
	}
	if _, err := ValidateGameDeleteTarget(aliasGame, nil, nil); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Errorf("symlink game: %v", err)
	}
	root := game
	root.Path = "/"
	if _, err := ValidateGameDeleteTarget(root, nil, nil); err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Errorf("root: %v", err)
	}
	manifest := filepath.Join(game.Path, "MicrosoftGame.config")
	os.Remove(manifest)
	os.Mkdir(manifest, 0o755)
	if _, err := ValidateGameDeleteTarget(game, nil, nil); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("directory manifest: %v", err)
	}
	os.Remove(manifest)
	if _, err := ValidateGameDeleteTarget(game, nil, nil); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("missing manifest: %v", err)
	}
}

func TestRemovalLocks(t *testing.T) {
	game := gameFixture(t)
	var others []shared.DownloadTask
	for _, status := range []string{"queued", "running", "paused", "complete", "failed", "cancelled"} {
		others = append(others, download(game.Path+"-other", status))
	}
	if err := AssertGameRemovalAvailable(game, others, func(shared.DownloadTask) bool { return true }, nil); err != nil {
		t.Errorf("unrelated downloads: %v", err)
	}
	for _, status := range []string{"queued", "running", "paused"} {
		for _, destination := range []string{game.Path, filepath.Dir(game.Path), filepath.Join(game.Path, "not-created", "dlc")} {
			if err := AssertGameRemovalAvailable(game, []shared.DownloadTask{download(destination, status)}, inactive, nil); err == nil || !strings.Contains(err.Error(), "using this folder") {
				t.Errorf("%s %s: %v", status, destination, err)
			}
		}
	}
	for _, kind := range []string{"install", "update", "content"} {
		task := download(game.Path+"-separate-content", "paused")
		task.GameID, task.Kind = game.ID, kind
		if err := AssertGameRemovalAvailable(game, []shared.DownloadTask{task}, inactive, nil); err == nil || !strings.Contains(err.Error(), "this game's installation") {
			t.Errorf("%s: %v", kind, err)
		}
	}
	for _, status := range []string{"complete", "failed", "cancelled"} {
		task := download(game.Path, status)
		if err := AssertGameRemovalAvailable(game, []shared.DownloadTask{task}, func(shared.DownloadTask) bool { return true }, nil); err == nil {
			t.Errorf("%s with active cleanup allowed", status)
		}
		if err := AssertGameRemovalAvailable(game, []shared.DownloadTask{task}, inactive, nil); err != nil {
			t.Errorf("%s: %v", status, err)
		}
	}
	alias := game.Path + "-alias"
	os.Symlink(game.Path, alias)
	for _, destination := range []string{alias, filepath.Join(alias, "future", "content")} {
		if err := AssertGameRemovalAvailable(game, []shared.DownloadTask{download(destination, "running")}, inactive, nil); err == nil {
			t.Errorf("alias %s allowed", destination)
		}
	}
	future := game.Path + "-future"
	os.Symlink(filepath.Join(filepath.Base(game.Path), "not-created"), future)
	if err := AssertGameRemovalAvailable(game, []shared.DownloadTask{download(filepath.Join(future, "content"), "running")}, inactive, nil); err == nil {
		t.Error("dangling link allowed")
	}
	if err := AssertGameRemovalAvailable(game, nil, inactive, &game); err == nil || !strings.Contains(err.Error(), "Close this game") {
		t.Errorf("running: %v", err)
	}
	for _, location := range []string{alias, filepath.Dir(game.Path), filepath.Join(game.Path, "bin")} {
		other := game
		other.ID, other.Path = "other", location
		if err := AssertGameRemovalAvailable(game, nil, inactive, &other); err == nil || !strings.Contains(err.Error(), "using this installation folder") {
			t.Errorf("running at %s: %v", location, err)
		}
	}
	sibling := game
	sibling.ID, sibling.Path = "other", game.Path+"-other"
	if err := AssertGameRemovalAvailable(game, nil, inactive, &sibling); err != nil {
		t.Errorf("sibling: %v", err)
	}
	missing := game
	missing.Path = game.Path + "-missing"
	if err := AssertGameRemovalAvailable(missing, nil, inactive, nil); err != nil {
		t.Errorf("forgetting a missing game: %v", err)
	}
	if _, err := ValidateGameDeleteTarget(missing, nil, nil); err == nil {
		t.Error("missing game deletable")
	}
}

func TestDeleteGameDirectory(t *testing.T) {
	game := gameFixture(t)
	root := filepath.Dir(game.Path)
	saves := filepath.Join(root, "outside-saves")
	os.Mkdir(saves, 0o755)
	os.WriteFile(filepath.Join(saves, "save.txt"), []byte("keep"), 0o644)
	os.Symlink(saves, filepath.Join(game.Path, "linked-saves"))
	target, err := ValidateGameDeleteTarget(game, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteGameDirectory(target, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(game.Path); err == nil {
		t.Error("game still exists")
	}
	if data, _ := os.ReadFile(filepath.Join(saves, "save.txt")); string(data) != "keep" {
		t.Error("linked saves deleted")
	}

	for _, kind := range []string{"directory", "symlink", "file", "missing"} {
		game := gameFixture(t)
		target, _ := ValidateGameDeleteTarget(game, nil, nil)
		old := filepath.Join(filepath.Dir(game.Path), "original")
		os.Rename(game.Path, old)
		switch kind {
		case "directory":
			os.Mkdir(game.Path, 0o755)
		case "symlink":
			os.Symlink(old, game.Path)
		case "file":
			os.WriteFile(game.Path, []byte("replacement"), 0o644)
		}
		called := false
		if err := DeleteGameDirectory(target, func(string) error { called = true; return nil }); err == nil || called {
			t.Errorf("%s replacement deleted", kind)
		}
	}

	game = gameFixture(t)
	target, _ = ValidateGameDeleteTarget(game, nil, nil)
	parent := filepath.Join(filepath.Dir(game.Path), "parent")
	os.Mkdir(parent, 0o755)
	moved := filepath.Join(parent, filepath.Base(game.Path))
	os.Rename(game.Path, moved)
	renamed := filepath.Join(filepath.Dir(game.Path), "renamed")
	os.Rename(parent, renamed)
	os.Symlink(renamed, parent)
	altered := target
	altered.Path = moved
	if err := DeleteGameDirectory(altered, func(string) error { t.Error("removed through a symlinked ancestor"); return nil }); err == nil || !strings.Contains(err.Error(), "folder changed") {
		t.Errorf("ancestor: %v", err)
	}

	game = gameFixture(t)
	target, _ = ValidateGameDeleteTarget(game, nil, nil)
	err = DeleteGameDirectory(target, func(directory string) error {
		os.Remove(filepath.Join(directory, "data.bin"))
		return errors.New("Permission denied")
	})
	if err == nil || !strings.Contains(err.Error(), "some files may already have been deleted") {
		t.Errorf("partial: %v", err)
	}
}
