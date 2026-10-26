package game

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

// --- game updates ---

var fourPart = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

func packageVersion(value string) []int {
	if !fourPart.MatchString(value) {
		return nil
	}
	parts := make([]int, 4)
	for i, part := range strings.Split(value, ".") {
		number, err := strconv.Atoi(part)
		if err != nil || number > 65535 {
			return nil
		}
		parts[i] = number
	}
	return parts
}

func xmlValue(xml, key string) (string, bool) {
	match := regexp.MustCompile("<" + key + ">([^<]*)</" + key + ">").FindStringSubmatch(xml)
	if match == nil {
		return "", false
	}
	return match[1], true
}

var digitsOnly = regexp.MustCompile(`^\d+$`)

func UpdateResult(xml, installed string) shared.GameUpdate {
	base := shared.GameUpdate{CheckedAt: shared.JSISOString(time.Now())}
	latest, _ := xmlValue(xml, "LatestVersion")
	left, right := packageVersion(installed), packageVersion(latest)
	hrText, _ := xmlValue(xml, "HResult")
	hr, hrOK := int64(0), false
	if digitsOnly.MatchString(hrText) {
		hr, _ = strconv.ParseInt(hrText, 10, 64)
		hrOK = true
	}
	checked, _ := xmlValue(xml, "Checked")
	stage, _ := xmlValue(xml, "ErrorStage")
	// The service deliberately rejects automatic mandatory/DLC policy for a
	// newer version. Its sanitized version-comparison result still establishes
	// availability from the genuine authenticated GetBasePackage response.
	if left == nil || right == nil || !hrOK || !((hr == 0 && checked == "true") || (hr == 0x80004001 && stage == "version-comparison")) {
		code := "unknown"
		if hrOK {
			code = fmt.Sprintf("%08x", hr)
		}
		base.Error = fmt.Sprintf("Update availability could not be verified (0x%s).", code)
		return base
	}
	comparison := 0
	for i := 0; i < 4 && comparison == 0; i++ {
		comparison = right[i] - left[i]
	}
	available := comparison > 0
	base.LatestVersion, base.Available = latest, &available
	return base
}

var storeID = regexp.MustCompile(`(?i)^[A-Z0-9]{12}$`)

const updateMagic = 0x58445358

// QueryUpdate asks the local Xodus service whether a newer package exists.
func QueryUpdate(ctx context.Context, socketPath, productID, installed string) (shared.GameUpdate, error) {
	if !storeID.MatchString(productID) || packageVersion(installed) == nil {
		return shared.GameUpdate{}, errors.New("A verified Store ID and four-part installed version are required.")
	}
	if ctx.Err() != nil {
		return shared.GameUpdate{}, errors.New("Update check cancelled.")
	}
	body := []byte("<Request><ProductId>" + productID + "</ProductId><InstalledVersion>" + installed + "</InstalledVersion></Request>")
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header, updateMagic)
	binary.LittleEndian.PutUint16(header[4:], 9)
	binary.LittleEndian.PutUint16(header[6:], uint16(len(body)))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return shared.GameUpdate{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(65 * time.Second))
	if _, err := conn.Write(append(header, body...)); err != nil {
		return shared.GameUpdate{}, err
	}
	var buffer []byte
	chunk := make([]byte, 4096)
	for {
		n, err := conn.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if len(buffer) > 65543 {
			return shared.GameUpdate{}, errors.New("Invalid update response size.")
		}
		if len(buffer) >= 8 {
			kind := binary.LittleEndian.Uint16(buffer[4:])
			if binary.LittleEndian.Uint32(buffer) != updateMagic || (kind != 9 && kind != 10) {
				return shared.GameUpdate{}, errors.New("Unsupported update service response.")
			}
			length := int(binary.LittleEndian.Uint16(buffer[6:]))
			if len(buffer) >= 8+length {
				return UpdateResult(string(buffer[8:8+length]), installed), nil
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return shared.GameUpdate{}, errors.New("Update check cancelled.")
			}
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return shared.GameUpdate{}, errors.New("Game update check timed out.")
			}
			if errors.Is(err, io.EOF) {
				return shared.GameUpdate{}, errors.New("The update service closed before replying.")
			}
			return shared.GameUpdate{}, err
		}
	}
}

// --- anti-cheat ---

// AntiCheatEnvironment clears inherited per-launch markers; the package's own
// launcher controls EAC. PROTON_EAC_RUNTIME selects the actual bridge and is kept.
func AntiCheatEnvironment(env launcherenv.Env) launcherenv.Env {
	result := env.Clone()
	delete(result, "EOS_USE_ANTICHEATCLIENTNULL")
	delete(result, "PROTON_EAC_LAUNCHER_PROCESS")
	return result
}

var vdfPath = regexp.MustCompile(`"path"\s*"((?:\\.|[^"\\])*)"`)
var vdfEscape = regexp.MustCompile(`\\([\\"])`)

// InstalledEACRuntime discovers an already installed official Steam tool; it
// never downloads or copies it.
func InstalledEACRuntime(steamRoot string) string {
	libraries := []string{steamRoot}
	if folders, err := os.ReadFile(filepath.Join(steamRoot, "steamapps", "libraryfolders.vdf")); err == nil {
		for _, match := range vdfPath.FindAllStringSubmatch(string(folders), -1) {
			folder := vdfEscape.ReplaceAllString(match[1], "$1")
			if filepath.IsAbs(folder) {
				libraries = append(libraries, folder)
			}
		}
	}
	for _, library := range libraries {
		root := filepath.Join(library, "steamapps", "common", "Proton EasyAntiCheat Runtime")
		complete := true
		for _, name := range []string{"easyanticheat_x64.dll", "easyanticheat_x64.so"} {
			info, err := os.Stat(filepath.Join(root, "v2", "lib64", name))
			if err != nil || !info.Mode().IsRegular() || syscall.Access(filepath.Join(root, "v2", "lib64", name), 4) != nil {
				complete = false
				break
			}
		}
		if complete {
			return root
		}
	}
	return ""
}

// --- managed prefix ---

// Only directory permissions are repaired, never files inside an existing
// prefix. Do this before Proton starts: its Wine bootstrap uses the same guard
// as a normal launch.
func privateDirectory(directory string) error {
	handle, err := os.OpenFile(directory, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer handle.Close()
	before, err := handle.Stat()
	if err != nil {
		return err
	}
	stat, _ := before.Sys().(*syscall.Stat_t)
	if !before.IsDir() || stat == nil || int(stat.Uid) != os.Geteuid() {
		return errors.New("The managed Proton prefix must be a directory owned by the current user.")
	}
	if before.Mode().Perm() != 0o700 {
		if err := handle.Chmod(0o700); err != nil {
			return err
		}
	}
	current, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	currentStat, _ := current.Sys().(*syscall.Stat_t)
	if current.Mode()&fs.ModeSymlink != 0 || currentStat == nil || currentStat.Dev != stat.Dev || currentStat.Ino != stat.Ino || current.Mode().Perm() != 0o700 {
		return errors.New("The managed Proton prefix changed while checking its permissions.")
	}
	return nil
}

func PrepareManagedPrefix(compatibilityDataPath string) error {
	if !filepath.IsAbs(compatibilityDataPath) {
		return errors.New("The managed Proton prefix path must be absolute.")
	}
	if err := os.MkdirAll(compatibilityDataPath, 0o700); err != nil {
		return err
	}
	if err := privateDirectory(compatibilityDataPath); err != nil {
		return err
	}
	prefix := filepath.Join(compatibilityDataPath, "pfx")
	// The pinned Proton initializes when creation_sync_guard is missing, not
	// when the pfx directory is missing. Precreating this directory is safe.
	if err := os.Mkdir(prefix, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return privateDirectory(prefix)
}

// --- VC++ runtime ---

// The Microsoft download is immutable; its hash is also embedded in the CDN
// path. The installer is never packaged or redistributed with the launcher.
const (
	VCRedistVersion = "14.44.35211.0"
	VCRedistSHA256  = "cc0ff0eb1dc3f5188ae6300faef32bf5beeba4bdd6e8e445a9184072096b713b"
)

var VCRedistURL = "https://download.visualstudio.microsoft.com/download/pr/bd1c8d9d-ba95-4eee-bc6e-df1fcc876373/" + strings.ToUpper(VCRedistSHA256) + "/VC_redist.x64.exe"

func VersionAtLeast(actual, minimum string) bool {
	parse := func(value string) ([]int, bool) {
		parts := strings.Split(value, ".")
		out := make([]int, len(parts))
		for i, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil {
				return nil, false
			}
			out[i] = number
		}
		return out, true
	}
	left, okLeft := parse(actual)
	right, okRight := parse(minimum)
	if !okLeft || !okRight {
		return false
	}
	for i := range max(len(left), len(right)) {
		var a, b int
		if i < len(left) {
			a = left[i]
		}
		if i < len(right) {
			b = right[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

var (
	vcSection   = regexp.MustCompile(`(?m)^\[Software\\\\Microsoft\\\\VisualStudio\\\\14\.0\\\\VC\\\\Runtimes\\\\x64\](?:\s|$)`)
	vcInstalled = regexp.MustCompile(`(?m)^"Installed"=dword:00000001$`)
	vcVersion   = regexp.MustCompile(`(?mi)^"Version"="v?([0-9.]+)"$`)
)

func InstalledVCRuntimeVersion(prefix string) string {
	data, err := os.ReadFile(filepath.Join(prefix, "system.reg"))
	if err != nil {
		return ""
	}
	registry := string(data)
	// Wine appends a numeric timestamp after the closing bracket.
	location := vcSection.FindStringIndex(registry)
	if location == nil {
		return ""
	}
	section := registry[location[0]:]
	if end := strings.Index(section[1:], "\n["); end >= 0 {
		section = section[:end+1]
	}
	if !vcInstalled.MatchString(section) {
		return ""
	}
	// Microsoft's redistributable may register either 14.x or v14.x.
	if match := vcVersion.FindStringSubmatch(section); match != nil {
		return match[1]
	}
	return ""
}

func fileSHA256(file string) string {
	handle, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer handle.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type progressWriter struct {
	downloaded int64
	total      int64
	report     func(downloaded, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.downloaded += int64(len(b))
	p.report(p.downloaded, p.total)
	return len(b), nil
}

// DownloadVerifiedVCRedist downloads the pinned installer into cacheRoot and
// verifies it. total is 0 when unknown.
func DownloadVerifiedVCRedist(ctx context.Context, cacheRoot string, onProgress func(downloaded, total int64)) (string, error) {
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return "", err
	}
	destination := filepath.Join(cacheRoot, "vc_redist.x64-"+VCRedistVersion+".exe")
	if fileSHA256(destination) == VCRedistSHA256 {
		return destination, nil
	}
	temporary := fmt.Sprintf("%s.part-%d-%d", destination, os.Getpid(), time.Now().UnixMilli())
	defer os.Remove(temporary)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, VCRedistURL, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("Microsoft VC++ download failed (HTTP %d).", response.StatusCode)
	}
	handle, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	progress := &progressWriter{total: max(0, response.ContentLength), report: onProgress}
	_, err = io.Copy(io.MultiWriter(handle, hash, progress), response.Body)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != VCRedistSHA256 {
		return "", errors.New("Microsoft VC++ download failed SHA-256 verification.")
	}
	if err := os.Rename(temporary, destination); err != nil {
		return "", err
	}
	return destination, nil
}
