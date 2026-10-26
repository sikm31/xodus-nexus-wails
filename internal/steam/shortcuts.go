// Package steam ports steam-shortcuts.ts, steam-live.ts and steam-artwork.ts:
// non-Steam game shortcuts in Steam's binary shortcuts.vdf.
package steam

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/shared"
)

type field struct {
	kind  byte
	key   string
	text  string
	num   uint32
	child []field
}

var empty = []byte("\x00shortcuts\x00\x08\x08")

// Large imported libraries can contain over 11,000 shortcuts. Independent
// byte/field/depth limits keep increased capacity from allowing unbounded input.
const (
	maxShortcutBytes  = 32 * 1024 * 1024
	maxShortcutFields = 500_000
)

func isPNG(data []byte) bool {
	return len(data) > 0 && len(data) <= 4*1024*1024 && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
}

var digitsOnly = regexp.MustCompile(`^\d+$`)

func parse(data []byte) ([]field, error) {
	if len(data) > maxShortcutBytes {
		return nil, errors.New("Steam shortcuts file is too large.")
	}
	offset, count := 0, 0
	text := func() (string, error) {
		end := bytes.IndexByte(data[offset:], 0)
		if end < 0 || end > 65536 {
			return "", errors.New("Invalid Steam shortcut string.")
		}
		raw := data[offset : offset+end]
		if !utf8.Valid(raw) {
			return "", errors.New("Invalid Steam shortcut encoding.")
		}
		offset += end + 1
		return string(raw), nil
	}
	var object func(depth int) ([]field, error)
	object = func(depth int) ([]field, error) {
		if depth > 8 {
			return nil, errors.New("Steam shortcuts nesting is too deep.")
		}
		var fields []field
		for offset < len(data) {
			kind := data[offset]
			offset++
			if kind == 8 {
				return fields, nil
			}
			count++
			if count > maxShortcutFields {
				return nil, errors.New("Too many Steam shortcut fields.")
			}
			key, err := text()
			if err != nil {
				return nil, err
			}
			value := field{kind: kind, key: key}
			switch kind {
			case 0:
				if value.child, err = object(depth + 1); err != nil {
					return nil, err
				}
			case 1:
				if value.text, err = text(); err != nil {
					return nil, err
				}
			case 2, 3, 4, 6, 7, 10:
				size := 4
				if kind == 7 || kind == 10 {
					size = 8
				}
				if offset+size > len(data) {
					return nil, errors.New("Truncated Steam shortcuts file.")
				}
				value.num = binary.LittleEndian.Uint32(data[offset:])
				offset += size
			default:
				return nil, errors.New("Unsupported Steam shortcuts format. Original file was not changed.")
			}
			fields = append(fields, value)
		}
		return nil, errors.New("Truncated Steam shortcuts file.")
	}
	root, err := object(0)
	if err != nil {
		return nil, err
	}
	if offset != len(data) || len(root) != 1 || root[0].kind != 0 || root[0].key != "shortcuts" {
		return nil, errors.New("Invalid Steam shortcuts root.")
	}
	entries := root[0].child
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.kind != 0 || !digitsOnly.MatchString(entry.key) || seen[entry.key] {
			return nil, errors.New("Invalid Steam shortcut entry.")
		}
		seen[entry.key] = true
	}
	return entries, nil
}

func lookup(fields []field, key string) (field, bool) {
	for _, f := range fields {
		if strings.EqualFold(f.key, key) {
			return f, true
		}
	}
	return field{}, false
}

func stringField(key, value string) []byte { return []byte("\x01" + key + "\x00" + value + "\x00") }

func intField(key string, value uint32) []byte {
	out := []byte("\x02" + key + "\x00")
	return binary.LittleEndian.AppendUint32(out, value)
}

var unsafeArgument = regexp.MustCompile("[\x00\r\n]")
var argumentEscapes = regexp.MustCompile("[\"`$\\\\]")

// Argument quotes a value for a Steam shortcut field.
func Argument(value string) (string, error) {
	if unsafeArgument.MatchString(value) || strings.Contains(value, "%command%") {
		return "", errors.New("This path cannot be used in a Steam shortcut.")
	}
	return `"` + argumentEscapes.ReplaceAllString(value, `\$0`) + `"`, nil
}

type Launch struct {
	Title, Executable, Options, Directory, Identity, Icon string
}

func sameWords(left field, ok bool, right string) bool {
	if !ok || left.kind != 1 {
		return false
	}
	a, errA := shared.CommandWords(left.text)
	b, errB := shared.CommandWords(right)
	if errA != nil || errB != nil {
		return left.text == right
	}
	encodedA, _ := json.Marshal(a)
	encodedB, _ := json.Marshal(b)
	return string(encodedA) == string(encodedB)
}

// FindLaunches returns Steam-assigned shortcut IDs that run launch.
func FindLaunches(data []byte, launch Launch) ([]uint32, error) {
	entries, err := parse(data)
	if err != nil {
		return nil, err
	}
	var matches []uint32
	for _, entry := range entries {
		exe, exeOK := lookup(entry.child, "Exe")
		options, optionsOK := lookup(entry.child, "LaunchOptions")
		if !sameWords(exe, exeOK, launch.Executable) || !sameWords(options, optionsOK, launch.Options) {
			continue
		}
		if appid, ok := lookup(entry.child, "appid"); ok && appid.kind != 1 && appid.kind != 0 && appid.num&0x80000000 != 0 && !slices.Contains(matches, appid.num) {
			matches = append(matches, appid.num)
		}
	}
	return matches, nil
}

func crc32Steam(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc ^= uint32(b)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xedb88320
			} else {
				crc >>= 1
			}
		}
	}
	return crc ^ 0xffffffff
}

type Plan struct {
	Bytes []byte
	AppID uint32
	Added bool
}

// PlanShortcut adds launch to the shortcuts file, or finds its existing entry.
func PlanShortcut(original []byte, launch Launch) (Plan, error) {
	data := original
	if data == nil {
		data = empty
	}
	entries, err := parse(data)
	if err != nil {
		return Plan{}, err
	}
	for _, value := range []string{launch.Title, launch.Executable, launch.Options, launch.Directory, launch.Identity, launch.Icon} {
		if strings.ContainsRune(value, 0) || shared.UTF16Len(value) > 4096 {
			return Plan{}, errors.New("Invalid Steam shortcut data.")
		}
	}
	for _, entry := range entries {
		exe, exeOK := lookup(entry.child, "exe")
		options, optionsOK := lookup(entry.child, "LaunchOptions")
		if exeOK && exe.kind == 1 && exe.text == launch.Executable && optionsOK && options.kind == 1 && options.text == launch.Options {
			appid, ok := lookup(entry.child, "appid")
			if !ok || appid.kind == 0 || appid.kind == 1 || appid.num&0x80000000 == 0 {
				return Plan{}, errors.New("Invalid existing Steam shortcut ID.")
			}
			return Plan{Bytes: data, AppID: appid.num}, nil
		}
	}
	// Steam stores the ID explicitly. Include the profile/game identity so
	// games with the same title and shared /bin/sh executable cannot collide.
	appid := crc32Steam([]byte(launch.Executable+launch.Title+launch.Identity)) | 0x80000000
	index := int64(-1)
	for _, entry := range entries {
		if existing, ok := lookup(entry.child, "appid"); ok && existing.kind != 0 && existing.kind != 1 && existing.num == appid {
			return Plan{}, errors.New("Steam shortcut ID collision. No existing entry was changed.")
		}
		if value, err := strconv.ParseInt(entry.key, 10, 64); err == nil && value > index {
			index = value
		}
	}
	index++
	startDir, err := Argument(launch.Directory)
	if err != nil {
		return Plan{}, err
	}
	var block bytes.Buffer
	block.WriteString("\x00" + strconv.FormatInt(index, 10) + "\x00")
	for _, part := range [][]byte{intField("appid", appid), stringField("AppName", launch.Title), stringField("Exe", launch.Executable),
		stringField("StartDir", startDir), stringField("icon", launch.Icon), stringField("ShortcutPath", ""), stringField("LaunchOptions", launch.Options),
		intField("IsHidden", 0), intField("AllowDesktopConfig", 1), intField("AllowOverlay", 1), intField("OpenVR", 0), intField("Devkit", 0),
		stringField("DevkitGameID", ""), intField("DevkitOverrideAppID", 0), intField("LastPlayTime", 0), stringField("FlatpakAppID", ""),
		[]byte("\x00tags\x00\x010\x00Xodus Nexus\x00\x08\x08")} {
		block.Write(part)
	}
	updated := append(append(slices.Clone(data[:len(data)-2]), block.Bytes()...), data[len(data)-2:]...)
	if _, err := parse(updated); err != nil {
		return Plan{}, err
	}
	return Plan{Bytes: updated, AppID: appid, Added: true}, nil
}

// RunURI is the steam:// URL that starts a shortcut.
func RunURI(appid uint32) string {
	value := new(big.Int).Lsh(big.NewInt(int64(appid)), 32)
	value.Or(value, big.NewInt(0x02000000))
	return "steam://rungameid/" + value.String()
}

var steamProcess = regexp.MustCompile(`(?i)^(steam|steamwebhelper)$`)

// Running reports a Steam client process owned by this user.
func Running() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !digitsOnly.MatchString(entry.Name()) {
			continue
		}
		info, err := os.Stat("/proc/" + entry.Name())
		if err != nil {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
			continue
		}
		comm, err := os.ReadFile("/proc/" + entry.Name() + "/comm")
		if err == nil && steamProcess.MatchString(strings.TrimSpace(string(comm))) {
			return true
		}
	}
	return false
}

func owned(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid() && info.Mode().Perm()&0o022 == 0
}

func safeDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || !owned(info) {
		return errors.New("Unsafe Steam configuration directory.")
	}
	return nil
}

// ReadShortcuts reads shortcuts.vdf safely; nil when the file does not exist.
func ReadShortcuts(file string) ([]byte, error) {
	handle, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	before, err := handle.Stat()
	if err != nil {
		return nil, err
	}
	stat, _ := before.Sys().(*syscall.Stat_t)
	if !before.Mode().IsRegular() || stat == nil || stat.Nlink != 1 || !owned(before) || before.Size() > maxShortcutBytes {
		return nil, errors.New("Unsafe Steam shortcuts file.")
	}
	data := make([]byte, before.Size())
	if _, err := io.ReadFull(handle, data); err != nil {
		return nil, errors.New("Steam shortcuts changed concurrently.")
	}
	after, err := handle.Stat()
	afterStat, _ := after.Sys().(*syscall.Stat_t)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || afterStat.Ctim != stat.Ctim {
		return nil, errors.New("Steam shortcuts changed concurrently.")
	}
	return data, nil
}

// Accounts lists the config directories of signed-in native Steam profiles.
func Accounts(home string) ([]string, error) {
	var accounts []string
	for _, candidate := range []string{filepath.Join(home, ".steam/steam"), filepath.Join(home, ".local/share/Steam"),
		filepath.Join(home, ".var/app/com.valvesoftware.Steam/.local/share/Steam")} {
		root, err := filepath.EvalSymlinks(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		userdata := filepath.Join(root, "userdata")
		if err := safeDirectory(root); err != nil {
			return nil, err
		}
		if err := safeDirectory(userdata); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		ids, err := os.ReadDir(userdata)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if !digitsOnly.MatchString(id.Name()) || id.Name() == "0" {
				continue
			}
			user := filepath.Join(userdata, id.Name())
			if err := safeDirectory(user); err != nil {
				return nil, err
			}
			config := filepath.Join(user, "config")
			if err := safeDirectory(config); err == nil {
				if !slices.Contains(accounts, config) {
					accounts = append(accounts, config)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
	}
	return accounts, nil
}

type Artwork struct{ Cover, Wide []byte }

// WriteArtwork stores grid images for a shortcut. Existing custom artwork is
// always preserved; exclusive creation never follows existing links.
func WriteArtwork(config string, appid uint32, artwork Artwork) error {
	if appid < 0x80000000 {
		return errors.New("Invalid Steam shortcut ID.")
	}
	if err := safeDirectory(config); err != nil {
		return err
	}
	grid := filepath.Join(config, "grid")
	if err := os.Mkdir(grid, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := safeDirectory(grid); err != nil {
		return err
	}
	wide := artwork.Wide
	if wide == nil {
		wide = artwork.Cover
	}
	for _, item := range []struct {
		name string
		data []byte
	}{{fmt.Sprintf("%dp.png", appid), artwork.Cover}, {fmt.Sprintf("%d.png", appid), wide}, {fmt.Sprintf("%d_hero.png", appid), wide}} {
		if !isPNG(item.data) {
			continue
		}
		handle, err := os.OpenFile(filepath.Join(grid, item.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = handle.Write(item.data)
		if closeErr := handle.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
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

// AddShortcut edits shortcuts.vdf while Steam is closed and returns the run URI.
func AddShortcut(config string, launch Launch, artwork Artwork, running func() bool) (string, error) {
	if running == nil {
		running = Running
	}
	stopped := func() error {
		if running() {
			return errors.New("Close Steam completely, then try Add to Steam again. Nexus will not close Steam for you.")
		}
		return nil
	}
	if err := stopped(); err != nil {
		return "", err
	}
	if err := safeDirectory(config); err != nil {
		return "", err
	}
	lockPath := filepath.Join(config, ".xodus-nexus-shortcuts.lock")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", errors.New("Another Steam shortcut update is in progress.")
	}
	file := filepath.Join(config, "shortcuts.vdf")
	temporary := file + ".nexus-" + install.NewUUID()
	defer func() {
		os.Remove(temporary)
		lock.Close()
		os.Remove(lockPath)
	}()
	original, err := ReadShortcuts(file)
	if err != nil {
		return "", err
	}
	planned, err := PlanShortcut(original, launch)
	if err != nil {
		return "", err
	}
	if err := stopped(); err != nil {
		return "", err
	}
	if err := WriteArtwork(config, planned.AppID, artwork); err != nil {
		return "", err
	}
	if planned.Added {
		if err := writeExclusive(temporary, planned.Bytes); err != nil {
			return "", err
		}
		if original != nil {
			if err := writeExclusive(file+".nexus-backup-"+install.NewUUID(), original); err != nil {
				return "", err
			}
		}
		current, err := ReadShortcuts(file)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(current, original) {
			return "", errors.New("Steam shortcuts changed concurrently. Try again after closing Steam.")
		}
		if err := stopped(); err != nil {
			return "", err
		}
		if original != nil {
			err = os.Rename(temporary, file)
		} else {
			err = os.Link(temporary, file)
		}
		if err != nil {
			return "", err
		}
	}
	return RunURI(planned.AppID), nil
}
