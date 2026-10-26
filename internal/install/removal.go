package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"xodus-nexus-wails/internal/shared"
)

// AssertGameRemovalAvailable includes queued/resumable writes and terminal
// rows whose process or metadata cleanup still holds its queue slot. Canonical
// paths also protect aliases and not-yet-created destinations through
// symlinked ancestors.
func AssertGameRemovalAvailable(game shared.Game, downloads []shared.DownloadTask, isActive func(shared.DownloadTask) bool, running *shared.Game) error {
	var pending []shared.DownloadTask
	for _, task := range downloads {
		if active(task.Status) || isActive(task) {
			pending = append(pending, task)
		}
	}
	if running != nil && running.ID == game.ID {
		return errors.New("Close this game before removing it.")
	}
	if slices.ContainsFunc(pending, func(task shared.DownloadTask) bool { return task.GameID == game.ID }) {
		return errors.New("Finish or cancel this game's installation before removing it.")
	}
	target, err := CanonicalInstallPath(game.Path)
	if err != nil {
		return err
	}
	if running != nil {
		other, err := CanonicalInstallPath(running.Path)
		if err != nil {
			return err
		}
		if PathsOverlap(target, other) {
			return errors.New("Close the game using this installation folder before removing it.")
		}
	}
	for _, task := range pending {
		destination, err := CanonicalInstallPath(task.Destination)
		if err != nil {
			return err
		}
		if PathsOverlap(target, destination) {
			return errors.New("Finish or cancel the installation using this folder before removing the game.")
		}
	}
	return nil
}

func active(status string) bool {
	return status == "queued" || status == "running" || status == "paused"
}

func within(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

type DeleteTarget struct {
	Path          string
	Device, Inode string
}

var gameManifest = regexp.MustCompile(`(?i)^(MicrosoftGame\.config|AppxManifest\.xml)$`)

func ValidateGameDeleteTarget(game shared.Game, others []shared.Game, protected []string) (DeleteTarget, error) {
	target, err := filepath.Abs(game.Path)
	if err != nil {
		return DeleteTarget{}, err
	}
	if target == "/" {
		return DeleteTarget{}, errors.New("Refusing to remove a filesystem root.")
	}
	info, err := os.Lstat(target)
	if err != nil {
		return DeleteTarget{}, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return DeleteTarget{}, errors.New("The game location must be a real directory.")
	}
	if real, err := filepath.EvalSymlinks(target); err != nil || real != target {
		return DeleteTarget{}, errors.New("The game location contains a symbolic link.")
	}
	for _, entry := range protected {
		location, err := CanonicalInstallPath(entry)
		if err != nil {
			return DeleteTarget{}, err
		}
		if within(target, location) {
			return DeleteTarget{}, errors.New("The game location contains a protected launcher or system directory.")
		}
	}
	for _, other := range others {
		location, err := CanonicalInstallPath(other.Path)
		if err != nil {
			return DeleteTarget{}, err
		}
		if PathsOverlap(target, location) {
			return DeleteTarget{}, errors.New("The game location overlaps another library game.")
		}
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return DeleteTarget{}, err
	}
	index := slices.IndexFunc(entries, func(entry os.DirEntry) bool { return gameManifest.MatchString(entry.Name()) })
	if index < 0 {
		return DeleteTarget{}, errors.New("The game manifest is missing; remove this entry from the library instead.")
	}
	if manifest, err := os.Lstat(filepath.Join(target, entries[index].Name())); err != nil || !manifest.Mode().IsRegular() {
		return DeleteTarget{}, errors.New("The game manifest is missing; remove this entry from the library instead.")
	}
	device, inode := deviceInode(info)
	return DeleteTarget{Path: target, Device: device, Inode: inode}, nil
}

// DeleteGameDirectory is called only after explicit permanent-delete
// confirmation and validation, while the caller holds the removal lock. It
// never follows a replaced root.
func DeleteGameDirectory(target DeleteTarget, remove func(string) error) error {
	if remove == nil {
		remove = os.RemoveAll
	}
	info, err := os.Lstat(target.Path)
	changed := errors.New("The game folder changed before deletion. Nothing was deleted. Try removing the game again.")
	if err != nil {
		return changed
	}
	device, inode := deviceInode(info)
	if real, rerr := filepath.EvalSymlinks(target.Path); !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || device != target.Device || inode != target.Inode || rerr != nil || real != target.Path {
		return changed
	}
	if err := remove(target.Path); err != nil {
		return fmt.Errorf("Could not completely delete the game folder. The library entry was kept, but some files may already have been deleted. %s", err.Error())
	}
	return nil
}
