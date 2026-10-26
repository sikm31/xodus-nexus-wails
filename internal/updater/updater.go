// Package updater ports launcher-update.ts: verified in-place AppImage updates
// with a rollback journal.
//
// The original launcher updates itself from its own GitHub releases. This
// build has its own release source, so updates stay disabled until Config
// names a repository that publishes this launcher's AppImages.
package updater

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/shared"
)

type Config struct {
	// Repository is "owner/name" on GitHub; empty disables in-app updates.
	Repository string
	// AssetName returns the AppImage file name for a version.
	AssetName func(version string) string
}

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type Release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

type Candidate struct {
	Version         string
	Image, Checksum Asset
	ReleaseURL      string
}

var stableTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// SelectCandidate picks the newest verified stable release newer than current.
func SelectCandidate(config Config, releases []Release, current string) *Candidate {
	var stable []Release
	for _, release := range releases {
		if !release.Draft && !release.Prerelease && stableTag.MatchString(release.TagName) {
			stable = append(stable, release)
		}
	}
	slices.SortStableFunc(stable, func(a, b Release) int {
		switch {
		case shared.IsNewerVersion(a.TagName, b.TagName):
			return -1
		case shared.IsNewerVersion(b.TagName, a.TagName):
			return 1
		}
		return 0
	})
	assetURL := func(url, tag, name string) bool {
		return url == fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", config.Repository, tag, name)
	}
	for _, release := range stable {
		if !shared.IsNewerVersion(release.TagName, current) {
			continue
		}
		name := config.AssetName(release.TagName[1:])
		find := func(want string) (Asset, bool) {
			index := slices.IndexFunc(release.Assets, func(a Asset) bool { return a.Name == want })
			if index < 0 {
				return Asset{}, false
			}
			return release.Assets[index], true
		}
		image, imageOK := find(name)
		checksum, checksumOK := find(name + ".sha512sum")
		if !imageOK || !checksumOK || !assetURL(image.URL, release.TagName, image.Name) || !assetURL(checksum.URL, release.TagName, checksum.Name) ||
			image.Size < 64 || image.Size > 1<<30 || checksum.Size < 1 || checksum.Size > 4096 {
			continue
		}
		return &Candidate{Version: release.TagName[1:], Image: image, Checksum: checksum,
			ReleaseURL: fmt.Sprintf("https://github.com/%s/releases/tag/%s", config.Repository, release.TagName)}
	}
	return nil
}

type journal struct {
	Target          string `json:"target"`
	Backup          string `json:"backup"`
	PreviousVersion string `json:"previousVersion"`
	Version         string `json:"version"`
}

// Fetch performs an HTTP GET.
type Fetch func(ctx context.Context, url string, header http.Header) (*http.Response, error)

func HTTPFetch(ctx context.Context, url string, header http.Header) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for key, values := range header {
		request.Header[key] = values
	}
	return http.DefaultClient.Do(request)
}

type Updater struct {
	config   Config
	version  string
	image    string
	userData string
	changed  func()
	fetch    Fetch

	mu         sync.Mutex
	state      shared.LauncherUpdate
	targetPath string
	candidate  *Candidate
	journal    *journal
	checking   chan struct{}
	cancel     context.CancelFunc
}

func New(config Config, version, image, userData string, changed func(), fetch Fetch) *Updater {
	if fetch == nil {
		fetch = HTTPFetch
	}
	return &Updater{config: config, version: version, image: image, userData: userData, changed: changed, fetch: fetch,
		state: shared.LauncherUpdate{CurrentVersion: version}}
}

func (u *Updater) State() shared.LauncherUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	return shared.Clone(u.state)
}

func (u *Updater) update(change func(*shared.LauncherUpdate)) {
	u.mu.Lock()
	change(&u.state)
	u.mu.Unlock()
	u.changed()
}

func (u *Updater) Cancel() {
	u.mu.Lock()
	if u.cancel != nil {
		u.cancel()
	}
	u.mu.Unlock()
}

func owned(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func (u *Updater) writableImage() (string, error) {
	if u.config.Repository == "" {
		return "", errors.New("In-app updates are not configured for this build.")
	}
	if u.image == "" || !filepath.IsAbs(u.image) || !strings.HasSuffix(u.image, ".AppImage") {
		return "", errors.New("In-app updates require a writable AppImage. Use the installer for this build.")
	}
	target, err := filepath.EvalSymlinks(u.image)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("The AppImage must be a regular file owned and writable only by your user.")
	}
	if syscall.Access(target, 2) != nil || syscall.Access(filepath.Dir(target), 2) != nil {
		return "", errors.New("The AppImage must be a regular file owned and writable only by your user.")
	}
	return target, nil
}

func (u *Updater) journalFile() string { return filepath.Join(u.userData, "launcher-update.json") }

func (u *Updater) Initialize() {
	target, err := u.writableImage()
	u.mu.Lock()
	u.targetPath, u.state.CanInstall = target, err == nil
	u.mu.Unlock()
	data, err := os.ReadFile(u.journalFile())
	if err != nil {
		return
	}
	var value journal
	if json.Unmarshal(data, &value) != nil || value.Target != target || filepath.Dir(value.Backup) != filepath.Dir(value.Target) ||
		!strings.HasPrefix(filepath.Base(value.Backup), filepath.Base(value.Target)+".nexus-backup-") {
		return
	}
	if info, err := os.Lstat(value.Backup); err != nil || !info.Mode().IsRegular() || !owned(info) {
		return
	}
	u.mu.Lock()
	u.journal, u.state.CanRollback = &value, true
	u.mu.Unlock()
}

// Check looks for a newer release. Concurrent calls share one request.
func (u *Updater) Check(ctx context.Context) {
	u.mu.Lock()
	if u.state.Updating || u.state.RestartRequired {
		u.mu.Unlock()
		return
	}
	if u.checking != nil {
		done := u.checking
		u.mu.Unlock()
		<-done
		return
	}
	done := make(chan struct{})
	u.checking = done
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.checking = nil
		u.mu.Unlock()
		close(done)
	}()
	u.update(func(s *shared.LauncherUpdate) { s.Checking, s.Error = true, "" })
	candidate, err := u.performCheck(ctx)
	u.update(func(s *shared.LauncherUpdate) {
		s.Checking, s.CheckedAt = false, shared.JSISOString(time.Now())
		if err != nil {
			s.Error = err.Error()
			return
		}
		u.candidate = candidate
		s.LatestVersion, s.ReleaseURL = "", ""
		if candidate != nil {
			s.LatestVersion, s.ReleaseURL = candidate.Version, candidate.ReleaseURL
		}
	})
}

func (u *Updater) performCheck(ctx context.Context) (*Candidate, error) {
	if u.config.Repository == "" {
		return nil, errors.New("In-app updates are not configured for this build.")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	response, err := u.fetch(ctx, "https://api.github.com/repos/"+u.config.Repository+"/releases?per_page=100", http.Header{"Accept": {"application/vnd.github+json"}})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("GitHub update check returned HTTP %d.", response.StatusCode)
	}
	var releases []Release
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&releases); err != nil || releases == nil {
		return nil, errors.New("Invalid GitHub release list.")
	}
	return SelectCandidate(u.config, releases, u.version), nil
}

var checksumLine = regexp.MustCompile(`(?i)^([a-f0-9]{128})  ([A-Za-z0-9._-]+)$`)

func (u *Updater) lock(lockPath string) error {
	handle, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		info, err := os.Lstat(lockPath)
		if err != nil || !info.Mode().IsRegular() || !owned(info) {
			return errors.New("Unsafe launcher update lock.")
		}
		data, _ := os.ReadFile(lockPath)
		pid, err := strconv.Atoi(string(data))
		if err != nil || pid < 1 {
			return errors.New("Invalid launcher update lock.")
		}
		if err := syscall.Kill(pid, 0); err == nil || !errors.Is(err, syscall.ESRCH) {
			return errors.New("Another launcher update is active.")
		}
		os.Remove(lockPath)
		handle, err = os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = handle.WriteString(strconv.Itoa(os.Getpid()))
	handle.Close()
	return err
}

func isAppImage(header []byte) bool {
	return len(header) >= 64 && bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) && header[4] == 2 && header[5] == 1 &&
		binary.LittleEndian.Uint16(header[18:]) == 62 && bytes.Equal(header[8:11], []byte{0x41, 0x49, 0x02})
}

// Install downloads, verifies and swaps in the new AppImage, keeping a backup.
func (u *Updater) Install() (err error) {
	u.mu.Lock()
	if u.state.Updating || u.state.RestartRequired {
		u.mu.Unlock()
		return errors.New("A launcher update is already pending.")
	}
	candidate := u.candidate
	previous := u.journal
	u.mu.Unlock()
	if candidate == nil {
		return errors.New("Check for a newer launcher release first.")
	}
	target, err := u.writableImage()
	if err != nil {
		return err
	}
	lockPath := target + ".nexus-update.lock"
	if err := u.lock(lockPath); err != nil {
		return err
	}
	defer os.Remove(lockPath)
	before, err := os.Stat(target)
	if err != nil {
		return err
	}
	temporary := target + ".nexus-update-" + install.NewUUID()
	backup := target + ".nexus-backup-" + install.NewUUID()
	replaced, backedUp, installed := false, false, false
	ctx, cancel := context.WithCancel(context.Background())
	u.mu.Lock()
	u.cancel = cancel
	u.mu.Unlock()
	zero := 0.0
	u.update(func(s *shared.LauncherUpdate) {
		s.Updating, s.Error, s.Progress, s.Phase = true, "", &zero, "Downloading launcher update…"
	})
	defer func() {
		if err != nil {
			if replaced && backedUp {
				os.Rename(backup, target)
				backedUp = false
			}
		}
		os.Remove(temporary)
		if backedUp && !installed {
			os.Remove(backup)
		}
		cancel()
		u.mu.Lock()
		u.cancel = nil
		u.mu.Unlock()
		u.update(func(s *shared.LauncherUpdate) {
			s.Updating = false
			if err != nil {
				s.Error = err.Error()
			}
		})
	}()
	checksumCtx, stopChecksum := context.WithTimeout(ctx, 15*time.Second)
	response, err := u.fetch(checksumCtx, candidate.Checksum.URL, nil)
	if err != nil {
		stopChecksum()
		return err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	response.Body.Close()
	stopChecksum()
	if response.StatusCode < 200 || response.StatusCode > 299 || readErr != nil || len(body) > 4096 {
		return errors.New("Could not download the launcher checksum.")
	}
	match := checksumLine.FindStringSubmatch(strings.TrimSpace(string(body)))
	if match == nil || match[2] != candidate.Image.Name {
		return errors.New("Invalid launcher checksum file.")
	}
	imageCtx, stopImage := context.WithTimeout(ctx, 10*time.Minute)
	defer stopImage()
	response, err = u.fetch(imageCtx, candidate.Image.URL, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return errors.New("Could not download the launcher AppImage.")
	}
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	hash := sha512.New()
	var header []byte
	written := int64(0)
	chunk := make([]byte, 256*1024)
	for {
		n, readErr := response.Body.Read(chunk)
		if n > 0 {
			written += int64(n)
			if written > candidate.Image.Size {
				handle.Close()
				return errors.New("Launcher download exceeded its published size.")
			}
			if len(header) < 64 {
				header = append(header, chunk[:min(n, 64-len(header))]...)
			}
			hash.Write(chunk[:n])
			if _, err := handle.Write(chunk[:n]); err != nil {
				handle.Close()
				return err
			}
			progress := math.Round(float64(written) / float64(candidate.Image.Size) * 100)
			u.update(func(s *shared.LauncherUpdate) { s.Progress = &progress })
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			handle.Close()
			return readErr
		}
	}
	if err := handle.Sync(); err != nil {
		handle.Close()
		return err
	}
	handle.Close()
	if written != candidate.Image.Size || hex.EncodeToString(hash.Sum(nil)) != strings.ToLower(match[1]) {
		return errors.New("Launcher SHA-512 verification failed. Your current launcher was kept.")
	}
	if !isAppImage(header) {
		return errors.New("The update is not an x86_64 AppImage.")
	}
	now, err := os.Stat(target)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	beforeStat, _ := before.Sys().(*syscall.Stat_t)
	nowStat, _ := now.Sys().(*syscall.Stat_t)
	if current, err := u.writableImage(); err != nil || current != target || nowStat.Ino != beforeStat.Ino || now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		return errors.New("The installed launcher changed during download. Retry the update.")
	}
	u.update(func(s *shared.LauncherUpdate) { s.Phase = "Installing verified launcher…" })
	if err := copyExclusive(target, backup); err != nil {
		return err
	}
	backedUp = true
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	replaced = true
	value := journal{Target: target, Backup: backup, PreviousVersion: u.version, Version: candidate.Version}
	if err := os.MkdirAll(u.userData, 0o755); err != nil {
		return err
	}
	data, _ := json.Marshal(value)
	journalTemp := u.journalFile() + "." + install.NewUUID()
	defer os.Remove(journalTemp)
	if err := writeExclusive(journalTemp, data); err != nil {
		return err
	}
	if err := os.Rename(journalTemp, u.journalFile()); err != nil {
		return err
	}
	installed = true
	u.mu.Lock()
	u.journal, u.targetPath = &value, target
	u.mu.Unlock()
	u.update(func(s *shared.LauncherUpdate) {
		s.RestartRequired, s.CanRollback, s.Phase = true, true, "Update installed. Restart to use it."
	})
	if previous != nil && previous.Backup != "" {
		os.Remove(previous.Backup)
	}
	return nil
}

func writeExclusive(file string, data []byte) error {
	handle, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = handle.Write(data)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	return err
}

func copyExclusive(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Rollback restores the AppImage saved by the last update.
func (u *Updater) Rollback() error {
	u.mu.Lock()
	value := u.journal
	updating := u.state.Updating
	u.mu.Unlock()
	if updating || value == nil {
		return errors.New("No launcher backup is available.")
	}
	if target, err := u.writableImage(); err != nil || target != value.Target {
		return errors.New("The launcher location has changed.")
	}
	if info, err := os.Lstat(value.Backup); err != nil || !info.Mode().IsRegular() || !owned(info) {
		return errors.New("The launcher backup is unsafe.")
	}
	if err := os.Rename(value.Backup, value.Target); err != nil {
		return err
	}
	os.Remove(u.journalFile())
	u.mu.Lock()
	u.journal = nil
	u.mu.Unlock()
	u.update(func(s *shared.LauncherUpdate) {
		s.CanRollback, s.RestartRequired, s.Phase = false, true, "Previous launcher restored. Restart to use it."
	})
	return nil
}
