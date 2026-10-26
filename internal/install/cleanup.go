package install

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"xodus-nexus-wails/internal/shared"
)

type proof struct {
	Schema    int    `json:"schema"`
	ID        string `json:"id"`
	Product   string `json:"product"`
	Directory string `json:"directory"`
	Device    string `json:"device"`
	Inode     string `json:"inode"`
	Marker    string `json:"marker"`
	Token     string `json:"token"`
}

var (
	uuidPattern  = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	digits       = regexp.MustCompile(`^\d+$`)
	mountEscapes = regexp.MustCompile(`\\([0-7]{3})`)
)

func NewUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func denied(reason string) shared.DownloadCancelOptions {
	return shared.DownloadCancelOptions{DeleteFilesAllowed: false, Reason: reason}
}

func statT(info fs.FileInfo) *syscall.Stat_t {
	stat, _ := info.Sys().(*syscall.Stat_t)
	return stat
}

func privateOwned(info fs.FileInfo) bool {
	stat := statT(info)
	return stat != nil && info.Mode().Perm()&0o077 == 0 && int(stat.Uid) == os.Getuid()
}

// Cleanup holds private proof that a download folder was created by this
// launcher. Proof is never a flag supplied by renderer/task JSON. Existing
// directories, updates, content and historical untracked installs fail closed.
type Cleanup struct {
	root       string
	removeTree func(string) error
	mountInfo  func() (string, error)
}

func NewCleanup(userData string, removeTree func(string) error, mountInfo func() (string, error)) *Cleanup {
	if removeTree == nil {
		removeTree = os.RemoveAll
	}
	if mountInfo == nil {
		mountInfo = func() (string, error) {
			data, err := os.ReadFile("/proc/self/mountinfo")
			return string(data), err
		}
	}
	return &Cleanup{root: filepath.Join(userData, "download-ownership"), removeTree: removeTree, mountInfo: mountInfo}
}

func (c *Cleanup) safeRoot() error {
	info, err := os.Lstat(c.root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !privateOwned(info) {
		return errors.New("Unsafe ownership directory.")
	}
	return nil
}

func (c *Cleanup) file(id string) (string, error) {
	if !uuidPattern.MatchString(id) {
		return "", errors.New("Untracked installation.")
	}
	return filepath.Join(c.root, id+".json"), nil
}

func (c *Cleanup) proof(task shared.DownloadTask) (proof, error) {
	if err := c.safeRoot(); err != nil {
		return proof{}, err
	}
	file, err := c.file(task.ID)
	if err != nil {
		return proof{}, err
	}
	handle, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return proof{}, err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return proof{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 || statT(info).Nlink != 1 || !privateOwned(info) {
		return proof{}, errors.New("Unsafe ownership record.")
	}
	data, err := io.ReadAll(handle)
	if err != nil {
		return proof{}, err
	}
	var value proof
	if err := json.Unmarshal(data, &value); err != nil {
		return proof{}, err
	}
	if value.Schema != 1 || value.ID != task.ID || value.Product != task.ProductID || value.Directory != task.Destination ||
		!digits.MatchString(value.Device) || !digits.MatchString(value.Inode) || !uuidPattern.MatchString(value.Token) ||
		value.Marker != ".nexus-download-"+value.Token {
		return proof{}, errors.New("Ownership mismatch.")
	}
	return value, nil
}

func deviceInode(info fs.FileInfo) (string, string) {
	stat := statT(info)
	return strconv.FormatUint(uint64(stat.Dev), 10), strconv.FormatUint(stat.Ino, 10)
}

func (c *Cleanup) verify(directory string, p proof) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	device, inode := deviceInode(info)
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || device != p.Device || inode != p.Inode {
		return errors.New("Installation folder changed.")
	}
	marker, err := os.OpenFile(filepath.Join(directory, p.Marker), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer marker.Close()
	metadata, err := marker.Stat()
	if err != nil {
		return err
	}
	content, err := io.ReadAll(io.LimitReader(marker, 257))
	if err != nil || !metadata.Mode().IsRegular() || statT(metadata).Nlink != 1 || metadata.Size() > 256 || string(content) != p.Token {
		return errors.New("Installation marker changed.")
	}
	return nil
}

func writeExclusive(file string, content []byte) error {
	handle, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = handle.Write(content)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (c *Cleanup) Prepare(task shared.DownloadTask) error {
	if task.Kind != "install" || task.GameID != "" {
		return os.MkdirAll(task.Destination, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(task.Destination), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(task.Destination, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	// Failure to record ownership must never make a pre-existing directory deletable.
	func() {
		if real, err := filepath.EvalSymlinks(task.Destination); err != nil || real != task.Destination {
			return
		}
		info, err := os.Lstat(task.Destination)
		if err != nil {
			return
		}
		device, inode := deviceInode(info)
		token := NewUUID()
		p := proof{Schema: 1, ID: task.ID, Product: task.ProductID, Directory: task.Destination, Device: device, Inode: inode,
			Marker: ".nexus-download-" + token, Token: token}
		if writeExclusive(filepath.Join(task.Destination, p.Marker), []byte(token)) != nil {
			return
		}
		if os.MkdirAll(c.root, 0o700) != nil || c.safeRoot() != nil {
			return
		}
		file, err := c.file(task.ID)
		if err != nil {
			return
		}
		encoded, _ := json.Marshal(p)
		writeExclusive(file, encoded)
	}()
	return nil
}

// InheritResume carries an existing private proof forward on internal resume
// admission, never claiming a directory just because a new queue entry names
// it. The old proof is retained so a failed resume remains cancellable.
func (c *Cleanup) InheritResume(previous, next shared.DownloadTask) {
	if previous.Status != "paused" || next.Status != "queued" || previous.ID == next.ID || previous.Kind != "install" || next.Kind != "install" ||
		previous.GameID != "" || next.GameID != "" || previous.ProductID != next.ProductID || previous.Destination != next.Destination ||
		previous.SelectedContentID != next.SelectedContentID || previous.SelectionMarket != next.SelectionMarket {
		return
	}
	// Missing, stale or untracked proof stays fail-closed, without blocking resume.
	p, err := c.proof(previous)
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(previous.Destination); err != nil || real != previous.Destination {
		return
	}
	if c.verify(previous.Destination, p) != nil {
		return
	}
	file, err := c.file(next.ID)
	if err != nil {
		return
	}
	p.ID = next.ID
	encoded, _ := json.Marshal(p)
	writeExclusive(file, encoded)
}

func (c *Cleanup) Options(task shared.DownloadTask, locked, roots []string) shared.DownloadCancelOptions {
	if task.Kind != "install" || task.GameID != "" {
		return denied("Updates and DLC keep their files to protect the installed game.")
	}
	if task.Status == "complete" {
		return denied("Completed installations cannot be deleted by cancelling a download.")
	}
	untracked := denied("Only a new installation folder created and tracked by this launcher can be deleted. Existing or untracked folders are kept.")
	p, err := c.proof(task)
	if err != nil {
		return untracked
	}
	directory, err := filepath.EvalSymlinks(task.Destination)
	if err != nil {
		return untracked
	}
	if directory != task.Destination {
		return denied("The installation folder changed or contains a symbolic link.")
	}
	if c.verify(directory, p) != nil {
		return untracked
	}
	for _, item := range locked {
		other, err := CanonicalInstallPath(item)
		if err != nil {
			return untracked
		}
		if PathsOverlap(directory, other) {
			return denied("This folder is used by a game or another installation.")
		}
	}
	for _, item := range roots {
		root, err := CanonicalInstallPath(item)
		if err != nil {
			return untracked
		}
		if contains(directory, root) {
			return denied("This is a protected installation or system location.")
		}
	}
	return shared.DownloadCancelOptions{DeleteFilesAllowed: true}
}

func (c *Cleanup) validateTree(directory, device string) error {
	// Same-device bind mounts also need explicit rejection, not just st_dev checks.
	mounts, err := c.mountInfo()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(mounts), "\n") {
		fields := strings.Split(line, " ")
		if len(fields) < 5 {
			continue
		}
		mount := mountEscapes.ReplaceAllStringFunc(fields[4], func(escape string) string {
			code, _ := strconv.ParseUint(escape[1:], 8, 8)
			return string(rune(code))
		})
		if mount != "" && contains(directory, mount) {
			return errors.New("Installation contains a mounted filesystem.")
		}
	}
	count := 0
	var visit func(string) error
	visit = func(current string) error {
		count++
		if count > 200_000 {
			return errors.New("Too many files to verify safely.")
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		currentDevice, _ := deviceInode(info)
		if currentDevice != device || info.Mode()&fs.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return errors.New("Unsafe link, mount or special file in installation.")
		}
		if info.IsDir() {
			entries, err := os.ReadDir(current)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := visit(filepath.Join(current, entry.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(directory)
}

// Remove deletes a tracked, cancelled installation folder after moving it
// into a quarantine directory next to it.
func (c *Cleanup) Remove(task shared.DownloadTask, locked, roots []string) (err error) {
	if options := c.Options(task, locked, roots); !options.DeleteFilesAllowed {
		return errors.New(options.Reason)
	}
	p, err := c.proof(task)
	if err != nil {
		return err
	}
	parent := filepath.Dir(task.Destination)
	handle, err := os.OpenFile(parent, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	anchor := fmt.Sprintf("/proc/self/fd/%d", handle.Fd())
	var quarantine, moved string
	deletionStarted := false
	defer func() {
		if quarantine != "" && moved == "" {
			os.Remove(quarantine)
		}
		handle.Close()
	}()
	fail := func(cause error) error {
		recovery := ""
		if moved != "" {
			// Never overwrite another directory created at the original destination.
			source := filepath.Join(anchor, filepath.Base(task.Destination))
			if _, statErr := os.Lstat(source); statErr == nil {
				real, _ := filepath.EvalSymlinks(moved)
				recovery = fmt.Sprintf(" Remaining files are preserved at %s.", real)
			} else if errors.Is(statErr, fs.ErrNotExist) && os.Rename(moved, source) == nil {
				moved = ""
			} else {
				recovery = " Remaining files are preserved in the temporary cancellation folder next to the installation."
			}
		}
		prefix := "Files were not deleted."
		if deletionStarted {
			prefix = "Cleanup was incomplete; some downloaded files may already have been deleted."
		}
		return fmt.Errorf("%s%s %s", prefix, recovery, cause.Error())
	}
	if real, err := filepath.EvalSymlinks(anchor); err != nil || real != parent {
		return fail(errors.New("Installation parent changed."))
	}
	source := filepath.Join(anchor, filepath.Base(task.Destination))
	if err := c.verify(source, p); err != nil {
		return fail(err)
	}
	if quarantine, err = os.MkdirTemp(anchor, ".nexus-cancel-"); err != nil {
		quarantine = ""
		return fail(err)
	}
	if err := os.Rename(source, filepath.Join(quarantine, "files")); err != nil {
		return fail(err)
	}
	moved = filepath.Join(quarantine, "files")
	// An atomic rename cannot accidentally delete a replacement at the old path.
	if err := c.verify(moved, p); err != nil {
		return fail(err)
	}
	real, err := filepath.EvalSymlinks(moved)
	if err != nil {
		return fail(err)
	}
	if err := c.validateTree(real, p.Device); err != nil {
		return fail(err)
	}
	if err := c.verify(moved, p); err != nil {
		return fail(err)
	}
	deletionStarted = true
	if err := c.removeTree(moved); err != nil {
		return fail(err)
	}
	moved = ""
	file, _ := c.file(task.ID)
	if err := os.Remove(file); err != nil {
		return fail(err)
	}
	return nil
}

// Complete drops ownership metadata. It must not turn a completed download into a failure.
func (c *Cleanup) Complete(task shared.DownloadTask) {
	p, err := c.proof(task)
	if err != nil || c.verify(task.Destination, p) != nil {
		return
	}
	os.Remove(filepath.Join(task.Destination, p.Marker))
	if file, err := c.file(task.ID); err == nil {
		os.Remove(file)
	}
}
