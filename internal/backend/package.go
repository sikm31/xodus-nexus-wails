// Package backend ports backend-package.ts, backend-releases.ts and
// backend.ts: discovery, verification, installation and source builds of the
// Xodus command-line backend.
package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"xodus-nexus-wails/assets"
)

// Repository publishes prebuilt backend releases.
const Repository = "domsvoboda/xodus-nexus"

type Patch struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type Lock struct {
	Schema          int     `json:"schema"`
	Version         string  `json:"version"`
	Repository      string  `json:"repository"`
	Commit          string  `json:"commit"`
	Rust            string  `json:"rust"`
	CargoLockSHA256 string  `json:"cargoLockSha256"`
	PatchID         string  `json:"patchId"`
	Compatibility   string  `json:"compatibility"`
	Patches         []Patch `json:"patches"`
}

var (
	// SourceLock is the pinned backend recipe.
	SourceLock = func() Lock {
		var lock Lock
		if err := json.Unmarshal(assets.BackendLock, &lock); err != nil {
			panic(err)
		}
		return lock
	}()
	RecipeSHA256 = func() string { sum := sha256.Sum256(assets.BackendLock); return hex.EncodeToString(sum[:]) }()
	// LinuxRunPatch is the backend launch protocol the launcher speaks.
	LinuxRunPatch = SourceLock.Compatibility
	// MinimumVersion: any stable backend release at or above the version the
	// launcher was built with is accepted, so backend updates do not require a
	// new launcher release.
	MinimumVersion = SourceLock.Version
)

func PackageName(version string) string { return "xodus-nexus-backend-" + version + "-linux-x86_64" }

func CompareVersions(a, b string) int {
	parse := func(value string) [3]int {
		var out [3]int
		for i, part := range strings.SplitN(value, ".", 3) {
			out[i], _ = strconv.Atoi(part)
		}
		return out
	}
	left, right := parse(a), parse(b)
	for i := range 3 {
		if left[i] != right[i] {
			return left[i] - right[i]
		}
	}
	return 0
}

type Manifest struct {
	Schema        int               `json:"schema"`
	Version       string            `json:"version"`
	Commit        string            `json:"commit"`
	RecipeSHA256  string            `json:"recipeSha256"`
	Compatibility string            `json:"compatibility"`
	Platform      string            `json:"platform"`
	Arch          string            `json:"arch"`
	Archive       string            `json:"archive"`
	ArchiveSHA512 string            `json:"archiveSha512"`
	Files         map[string]string `json:"files"`
	// AuthUI names the external sign-in webview protocol, not a UI toolkit.
	AuthUI string `json:"authUi"`
}

var (
	semver   = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	sha1Hex  = regexp.MustCompile(`^[a-f0-9]{40}$`)
	sha256Re = regexp.MustCompile(`^[a-f0-9]{64}$`)
	sha512Re = regexp.MustCompile(`^[a-f0-9]{128}$`)
)

func ValidateManifest(data []byte) (Manifest, error) {
	var item Manifest
	invalid := errors.New("The backend package manifest is not a valid compatible release.")
	if json.Unmarshal(data, &item) != nil {
		return Manifest{}, invalid
	}
	names := make([]string, 0, len(item.Files))
	for name := range item.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	if item.Schema != 1 || item.AuthUI != "electron" || !semver.MatchString(item.Version) || CompareVersions(item.Version, MinimumVersion) < 0 ||
		!sha1Hex.MatchString(item.Commit) || !sha256Re.MatchString(item.RecipeSHA256) || strings.TrimSpace(item.Compatibility) == "" ||
		item.Platform != "linux" || item.Arch != "x64" || item.Archive != PackageName(item.Version)+".tar.xz" || !sha512Re.MatchString(item.ArchiveSHA512) ||
		strings.Join(names, ",") != "LICENSE,xodus-cli,xodus-service" ||
		slices.ContainsFunc(names, func(name string) bool { return !sha256Re.MatchString(item.Files[name]) }) {
		return Manifest{}, invalid
	}
	return item, nil
}

func AssetURL(tag, name, actual string) (string, error) {
	expected := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", Repository, url.PathEscape(tag), name)
	if actual != expected {
		return "", errors.New("Unexpected backend release asset URL.")
	}
	return expected, nil
}

var verboseSize = regexp.MustCompile(`^[-d]\S*\s+\S+\s+(\d+)\s`)

// ValidateArchive permits only the package directory and regular binaries/license.
func ValidateArchive(entries, verbose []string, packageName string) error {
	if packageName == "" {
		packageName = PackageName(SourceLock.Version)
	}
	expected := map[string]bool{packageName + "/": true}
	for _, name := range []string{"xodus-cli", "xodus-service", "LICENSE"} {
		expected[packageName+"/"+name] = true
	}
	unsafe := errors.New("Unsafe backend archive: only the package directory and regular binaries/license are allowed.")
	if len(entries) != len(expected) || len(verbose) != len(entries) {
		return unsafe
	}
	for _, name := range entries {
		if !expected[name] {
			return unsafe
		}
		delete(expected, name)
	}
	for _, line := range verbose {
		match := verboseSize.FindStringSubmatch(line)
		if match == nil {
			return unsafe
		}
		if size, err := strconv.ParseInt(match[1], 10, 64); err != nil || size > 256*1024*1024 {
			return unsafe
		}
	}
	return nil
}
