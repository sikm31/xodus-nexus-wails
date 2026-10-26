package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"xodus-nexus-wails/internal/install"
	"xodus-nexus-wails/internal/proc"
)

type Artifact struct {
	Tag        string
	Manifest   Manifest
	ArchiveURL string
}

// Fetch opens an HTTP GET body.
type Fetch func(ctx context.Context, url string) (*http.Response, error)

func HTTPFetch(ctx context.Context, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(request)
}

func download(ctx context.Context, fetch Fetch, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	response, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("Backend download failed (HTTP %d).", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("Backend metadata exceeds the download limit.")
	}
	return data, nil
}

var releaseTag = regexp.MustCompile(`^backend-v(\d+\.\d+\.\d+)$`)

func Discover(ctx context.Context, fetch Fetch) (Artifact, error) {
	data, err := download(ctx, fetch, "https://api.github.com/repos/"+Repository+"/releases?per_page=100", 2_000_000)
	if err != nil {
		return Artifact{}, err
	}
	var releases []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(data, &releases); err != nil || releases == nil {
		return Artifact{}, errors.New("Invalid backend release list.")
	}
	selected := -1
	selectedVersion := ""
	for i, item := range releases {
		match := releaseTag.FindStringSubmatch(item.TagName)
		if item.Draft || item.Prerelease || match == nil || CompareVersions(match[1], MinimumVersion) < 0 {
			continue
		}
		if selected < 0 || CompareVersions(match[1], selectedVersion) > 0 {
			selected, selectedVersion = i, match[1]
		}
	}
	if selected < 0 {
		return Artifact{}, errors.New("No compatible Nexus backend release has been published yet.")
	}
	release := releases[selected]
	packageName := PackageName(selectedVersion)
	asset := func(name string) (string, error) {
		actual := ""
		for _, item := range release.Assets {
			if item.Name == name {
				actual = item.URL
				break
			}
		}
		return AssetURL(release.TagName, name, actual)
	}
	archive := packageName + ".tar.xz"
	manifestURL, err := asset(packageName + ".manifest.json")
	if err != nil {
		return Artifact{}, err
	}
	manifestData, err := download(ctx, fetch, manifestURL, 100_000)
	if err != nil {
		return Artifact{}, err
	}
	manifest, err := ValidateManifest(manifestData)
	if err != nil {
		return Artifact{}, err
	}
	if manifest.Version != selectedVersion {
		return Artifact{}, errors.New("The backend release does not match its manifest.")
	}
	checksumURL, err := asset(packageName + ".sha512sum")
	if err != nil {
		return Artifact{}, err
	}
	checksum, err := download(ctx, fetch, checksumURL, 1000)
	if err != nil {
		return Artifact{}, err
	}
	if strings.TrimSpace(string(checksum)) != manifest.ArchiveSHA512+"  "+archive {
		return Artifact{}, errors.New("Backend SHA-512 manifest mismatch.")
	}
	// Distribution must include the matching corresponding source, not just binaries.
	for _, name := range []string{packageName + "-source.tar.xz", packageName + "-source.sha512sum"} {
		if _, err := asset(name); err != nil {
			return Artifact{}, err
		}
	}
	archiveURL, err := asset(archive)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Tag: release.TagName, Manifest: manifest, ArchiveURL: archiveURL}, nil
}

// Runner runs a command for archive inspection.
type Runner func(command string, args []string, options proc.RunOptions) (proc.Result, error)

func verifyELF(file string) error {
	header := make([]byte, 64)
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	n, _ := io.ReadFull(handle, header)
	handle.Close()
	if n < 64 || !bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) || header[4] != 2 || header[5] != 1 || binary.LittleEndian.Uint16(header[18:]) != 62 {
		return errors.New("The backend binary is not a Linux x86_64 ELF executable.")
	}
	return nil
}

// Install downloads, verifies and unpacks a prebuilt backend. It returns the
// installed xodus-cli path.
func Install(ctx context.Context, managedRoot string, artifact Artifact, fetch Fetch, validate func(cli string) error,
	assertIdle func() error, phase func(string), runner Runner) (string, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return "", errors.New("The prebuilt backend currently supports x86_64 Linux only.")
	}
	if runner == nil {
		runner = proc.Run
	}
	if err := assertIdle(); err != nil {
		return "", err
	}
	encoded, _ := json.Marshal(artifact.Manifest)
	manifest, err := ValidateManifest(encoded)
	if err != nil {
		return "", err
	}
	staging := filepath.Join(managedRoot, ".download-"+install.NewUUID())
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	phase("Downloading the Nexus backend…")
	archive := filepath.Join(staging, manifest.Archive)
	if err := downloadArchive(ctx, fetch, artifact.ArchiveURL, archive, manifest.ArchiveSHA512, phase); err != nil {
		return "", err
	}
	listing, err := runner("tar", []string{"-tJf", archive}, proc.RunOptions{MaxOutput: 100_000})
	if err != nil {
		return "", err
	}
	verbose, err := runner("tar", []string{"-tvJf", archive}, proc.RunOptions{MaxOutput: 100_000})
	if err != nil {
		return "", err
	}
	if listing.Code != 0 || verbose.Code != 0 {
		return "", errors.New("The backend archive is invalid.")
	}
	packageName := PackageName(manifest.Version)
	if err := ValidateArchive(strings.Split(strings.TrimRight(listing.Output, " \t\r\n"), "\n"),
		strings.Split(strings.TrimRight(verbose.Output, " \t\r\n"), "\n"), packageName); err != nil {
		return "", err
	}
	extracted := filepath.Join(staging, "extracted")
	if err := os.Mkdir(extracted, 0o755); err != nil {
		return "", err
	}
	result, err := runner("tar", []string{"-xJf", archive, "--no-same-owner", "--no-same-permissions", "-C", extracted}, proc.RunOptions{})
	if err != nil {
		return "", err
	}
	if result.Code != 0 {
		return "", errors.New("Backend extraction failed.")
	}
	candidate := filepath.Join(extracted, packageName)
	verifyFiles := func(root string) error {
		for name, expected := range manifest.Files {
			target := filepath.Join(root, name)
			info, err := os.Lstat(target)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(target)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if !info.Mode().IsRegular() || hex.EncodeToString(sum[:]) != expected {
				return fmt.Errorf("Backend file verification failed: %s", name)
			}
		}
		for _, name := range []string{"xodus-cli", "xodus-service"} {
			file := filepath.Join(root, name)
			if err := verifyELF(file); err != nil {
				return err
			}
			if err := os.Chmod(file, 0o755); err != nil {
				return err
			}
		}
		// Never execute xodus-service --help: it starts keychain/device auth.
		return validate(filepath.Join(root, "xodus-cli"))
	}
	phase("Checking backend commands and runtime libraries…")
	if err := verifyFiles(candidate); err != nil {
		return "", err
	}
	if err := assertIdle(); err != nil {
		return "", err
	}
	release := filepath.Join(managedRoot, "releases", packageName+"-"+manifest.ArchiveSHA512[:16])
	if err := os.MkdirAll(filepath.Dir(release), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(candidate, release); err != nil {
		if !errors.Is(err, fs.ErrExist) && !strings.Contains(err.Error(), "directory not empty") {
			return "", err
		}
		if err := verifyFiles(release); err != nil {
			return "", err
		}
	}
	metadata, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(release, "metadata.json"), metadata, 0o644); err != nil {
		return "", err
	}
	return filepath.Join(release, "xodus-cli"), nil
}

func downloadArchive(ctx context.Context, fetch Fetch, url, destination, expected string, phase func(string)) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	response, err := fetch(ctx, url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("Backend download failed (HTTP %d).", response.StatusCode)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	hash := sha512.New()
	const limit = 256 * 1024 * 1024
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, limit+1))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if written > limit {
		return errors.New("Backend archive exceeds the download limit.")
	}
	phase("Verifying the backend checksum and archive…")
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errors.New("Backend SHA-512 verification failed.")
	}
	return nil
}
