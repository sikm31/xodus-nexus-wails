package steam

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
)

const sentinel = "/tmp/addnonsteamgamefile"

// RequestImport asks the running Steam client to import a .desktop file,
// matching SteamOS's native add-to-steam handshake. It never truncates or
// follows an unrelated file at the fixed sentinel path; Steam consumes it.
func RequestImport(file string) error {
	if !filepath.IsAbs(file) || strings.ContainsAny(file, "\x00\r\n") || !strings.HasSuffix(file, ".desktop") {
		return errors.New("Invalid Steam import launcher.")
	}
	source, err := os.Lstat(file)
	if err != nil || !source.Mode().IsRegular() || !owned(source) {
		return errors.New("Unsafe Steam import launcher.")
	}
	handle, err := os.OpenFile(sentinel, os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}
	info, err := handle.Stat()
	stat, _ := info.Sys().(*syscall.Stat_t)
	if err != nil || !info.Mode().IsRegular() || stat == nil || stat.Nlink != 1 || info.Size() != 0 || !owned(info) {
		handle.Close()
		return errors.New("Unsafe Steam import handshake file.")
	}
	now := time.Now()
	os.Chtimes(sentinel, now, now)
	handle.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/steam", "steam://addnonsteamgame/"+url.PathEscape(file))
	cmd.Env = launcherenv.External(nil).List()
	if err := cmd.Run(); err != nil {
		return errors.New("Could not send the shortcut to the running Steam client.")
	}
	return nil
}

var importing sync.Mutex

type match struct {
	config string
	appid  uint32
}

// AddLiveShortcut imports launch into a running Steam client. Steam owns its
// live shortcut database: the launcher reads back Steam's actual assigned ID
// before writing optional artwork and never manufactures a successful import.
// chooseExisting returns the selected profile, or "" to request a new import.
func AddLiveShortcut(configs []string, launch Launch, desktop string, artwork Artwork, request func(string) error,
	confirmation time.Duration, chooseExisting func(profiles []string) (string, error)) (string, error) {
	if !importing.TryLock() {
		return "", errors.New("Another Steam import is in progress. Please wait.")
	}
	defer importing.Unlock()
	if request == nil {
		request = RequestImport
	}
	var accounts []string
	for _, config := range configs {
		if !slices.Contains(accounts, config) {
			accounts = append(accounts, config)
		}
	}
	if len(accounts) == 0 {
		return "", errors.New("No native Steam profile found. Open Steam and sign in first.")
	}
	find := func(config string) ([]uint32, error) {
		data, err := ReadShortcuts(filepath.Join(config, "shortcuts.vdf"))
		if err != nil || data == nil {
			return nil, err
		}
		return FindLaunches(data, launch)
	}
	before := map[string][]uint32{}
	var existing []string
	for _, config := range accounts {
		ids, err := find(config)
		if err != nil {
			return "", err
		}
		before[config] = ids
		if len(ids) > 0 {
			existing = append(existing, config)
		}
	}
	if len(existing) > 0 {
		// A saved shortcut is not proof that its profile is currently signed
		// in. Let the user reuse it, or request a new import in Steam's current
		// session. Never infer identity from auto-login preferences.
		if chooseExisting == nil {
			return "", errors.New("A shortcut already exists. Choose a Steam profile to reuse it.")
		}
		selected, err := chooseExisting(existing)
		if err != nil {
			return "", err
		}
		if selected != "" {
			if !slices.Contains(existing, selected) {
				return "", errors.New("Invalid Steam profile selection.")
			}
			ids, err := find(selected)
			if err != nil || len(ids) == 0 {
				return "", errors.New("The existing Steam shortcut changed. Try Add to Steam again.")
			}
			if err := WriteArtwork(selected, ids[0], artwork); err != nil {
				return "", err
			}
			return RunURI(ids[0]), nil
		}
	}
	if err := request(desktop); err != nil {
		return "", err
	}
	deadline := time.Now().Add(min(15*time.Second, max(0, confirmation)))
	for {
		var added []match
		complete := true
		for _, config := range accounts {
			ids, err := find(config)
			if err != nil {
				// Steam may be mid-write. Retry reads only.
				complete = false
				continue
			}
			for _, id := range ids {
				if !slices.Contains(before[config], id) {
					added = append(added, match{config, id})
				}
			}
		}
		if complete && len(added) > 1 {
			var profiles []string
			for _, entry := range added {
				if !slices.Contains(profiles, entry.config) {
					profiles = append(profiles, entry.config)
				}
			}
			if chooseExisting != nil && len(profiles) == len(added) {
				selected, err := chooseExisting(profiles)
				if err != nil {
					return "", err
				}
				if index := slices.IndexFunc(added, func(entry match) bool { return entry.config == selected }); index >= 0 {
					if err := WriteArtwork(added[index].config, added[index].appid, artwork); err != nil {
						return "", err
					}
					return RunURI(added[index].appid), nil
				}
			}
			return "", errors.New("Steam changed more than one matching shortcut. Check Steam, then try Add to Steam again.")
		}
		if complete && len(added) == 1 {
			if err := WriteArtwork(added[0].config, added[0].appid, artwork); err != nil {
				return "", err
			}
			return RunURI(added[0].appid), nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", errors.New("Steam has not confirmed the shortcut yet. Check Steam, then try Add to Steam again.")
}
