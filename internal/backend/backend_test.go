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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"xodus-nexus-wails/internal/shared"
)

func elf() []byte {
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1})
	binary.LittleEndian.PutUint16(data[18:], 62)
	return data
}

func respond(body []byte) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func hexSum(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func fakeCLI(t *testing.T, path, streamingHelp, resolveHelp string, auth bool) {
	t.Helper()
	help := "xodus-cli 0.1.0\n  account    Account\n  login    Login\n  logout    Logout\n  download    Download\n  streaming    Streaming\n  run    Run\n  resolve-package    Resolve packages\n"
	if auth {
		help += "\nOptions:\n  --external-webview-stdio  External UI\n"
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"\"streaming --help\") printf '%s\\n' " + shellQuote(streamingHelp) + ";;\n" +
		"\"resolve-package --help\") printf '%s\\n' " + shellQuote(resolveHelp) + ";;\n" +
		"\"run --help\") printf -- '--materialize-chain\\n';;\n" +
		"*) printf '%s' " + shellQuote(help) + ";;\nesac\n"
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

func newManager(root string, busy func() bool, fetch Fetch) *Manager {
	m := NewManager(root, root, func(string, string) {}, busy, fetch)
	m.commandExists = func(string) bool { return false }
	m.externalDownload = func() bool { return false }
	return m
}

func TestCapabilityDetection(t *testing.T) {
	cases := []struct {
		streaming, resolve                            string
		progress, controls, limit, content, selection bool
	}{
		{"--selected-content-id", "--json", false, false, false, false, true},
		{"--selected-content-id\n--progress-json\n--control-stdin\n--download-limit-kib", "--json", true, true, true, false, true},
		{"--selected-content-id\n--progress-json\n--control-stdin", "--json", true, true, false, false, true},
		{"--selected-content-id", "--include-dlc", false, false, false, true, true},
	}
	for i, c := range cases {
		root := t.TempDir()
		fakeCLI(t, filepath.Join(root, "target", "release", "xodus-cli"), c.streaming, c.resolve, true)
		caps := newManager(root, func() bool { return false }, nil).Refresh().Capabilities
		if caps.StreamingProgressJSON != c.progress || caps.StreamingControls != c.controls || caps.StreamingBandwidthLimit != c.limit ||
			caps.ContentPackageSelection != c.content || caps.PackageSelection != c.selection {
			t.Errorf("case %d: %+v", i, caps)
		}
	}
}

type releaseFixture struct {
	manifest map[string]any
	archive  []byte
	name     string
}

func buildRelease(t *testing.T, scenario string) releaseFixture {
	build := t.TempDir()
	name := PackageName(SourceLock.Version)
	bundle := filepath.Join(build, name)
	os.MkdirAll(bundle, 0o755)
	cli := elf()
	if scenario == "bad-elf" {
		cli = []byte("not an ELF")
	}
	service := elf()
	if scenario == "bad-service-elf" {
		service = []byte("not an ELF")
	}
	os.WriteFile(filepath.Join(bundle, "xodus-cli"), cli, 0o755)
	os.WriteFile(filepath.Join(bundle, "xodus-service"), service, 0o755)
	os.WriteFile(filepath.Join(bundle, "LICENSE"), []byte("synthetic license fixture"), 0o644)
	files := map[string]string{}
	for _, file := range []string{"xodus-cli", "xodus-service", "LICENSE"} {
		data, _ := os.ReadFile(filepath.Join(bundle, file))
		files[file] = hexSum(data)
	}
	switch scenario {
	case "binary-digest":
		files["xodus-cli"] = strings.Repeat("0", 64)
	case "symlink":
		os.Remove(filepath.Join(bundle, "xodus-service"))
		os.Symlink("xodus-cli", filepath.Join(bundle, "xodus-service"))
	case "hardlink":
		os.Remove(filepath.Join(bundle, "xodus-service"))
		os.Link(filepath.Join(bundle, "xodus-cli"), filepath.Join(bundle, "xodus-service"))
	case "missing-service":
		os.Remove(filepath.Join(bundle, "xodus-service"))
	}
	archive := name + ".tar.xz"
	if output, err := exec.Command("tar", "-cJf", filepath.Join(build, archive), "-C", build, name).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, output)
	}
	data, _ := os.ReadFile(filepath.Join(build, archive))
	sum := sha512.Sum512(data)
	digest := hex.EncodeToString(sum[:])
	if scenario == "archive-digest" {
		digest = strings.Repeat("0", 128)
	}
	recipe := RecipeSHA256
	if scenario == "recipe" {
		recipe = "not-a-valid-hash"
	}
	return releaseFixture{name: name, archive: data, manifest: map[string]any{"schema": 1, "authUi": "electron", "version": SourceLock.Version,
		"commit": SourceLock.Commit, "compatibility": SourceLock.Compatibility, "recipeSha256": recipe, "platform": "linux", "arch": "x64",
		"archive": archive, "archiveSha512": digest, "files": files}}
}

func (f releaseFixture) fetch(downloading *atomic.Bool) Fetch {
	tag := "backend-v" + SourceLock.Version
	url := func(name string) string {
		return "https://github.com/" + Repository + "/releases/download/" + tag + "/" + name
	}
	var assets []any
	for _, suffix := range []string{".tar.xz", ".manifest.json", ".sha512sum", "-source.tar.xz", "-source.sha512sum"} {
		assets = append(assets, map[string]any{"name": f.name + suffix, "browser_download_url": url(f.name + suffix)})
	}
	return func(ctx context.Context, target string) (*http.Response, error) {
		switch {
		case strings.Contains(target, "/releases?"):
			body, _ := json.Marshal([]any{map[string]any{"tag_name": "v0.1.0", "assets": []any{}},
				map[string]any{"tag_name": tag, "prerelease": true, "assets": []any{}}, map[string]any{"tag_name": tag, "assets": assets}})
			return respond(body)
		case strings.HasSuffix(target, ".manifest.json"):
			body, _ := json.Marshal(f.manifest)
			return respond(body)
		case strings.HasSuffix(target, ".sha512sum"):
			return respond([]byte(f.manifest["archiveSha512"].(string) + "  " + f.manifest["archive"].(string) + "\n"))
		}
		downloading.Store(true)
		return respond(f.archive)
	}
}

func fullCapabilities() *shared.BackendCapabilities {
	return &shared.BackendCapabilities{Commands: []string{"account", "login", "logout", "download", "streaming", "run", "resolve-package"},
		Account: true, Login: true, Logout: true, Download: true, Streaming: true, Run: true, ExternalAuth: true, PackageSelection: true}
}

func TestInstallScenarios(t *testing.T) {
	for _, scenario := range []string{"valid", "archive-digest", "recipe", "binary-digest", "symlink", "hardlink", "missing-service", "bad-elf",
		"bad-service-elf", "missing-capability", "busy-after-download"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			fixture := buildRelease(t, scenario)
			var downloading atomic.Bool
			managed := filepath.Join(root, "xodus-backends")
			os.Mkdir(managed, 0o755)
			old := `{"previous":"keep intact"}`
			os.WriteFile(filepath.Join(managed, "current.json"), []byte(old), 0o644)
			m := newManager(root, func() bool { return scenario == "busy-after-download" && downloading.Load() }, fixture.fetch(&downloading))
			m.validate = func(binary string) *shared.BackendCapabilities {
				if _, err := os.Stat(binary); err != nil {
					return nil
				}
				return fullCapabilities()
			}
			m.validateRelease = func(string) error {
				if scenario == "missing-capability" {
					return errors.New("Backend validation failed.")
				}
				return nil
			}
			err := m.Update(context.Background(), func() {})
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				var current pointer
				data, _ := os.ReadFile(filepath.Join(managed, "current.json"))
				json.Unmarshal(data, &current)
				if current.Delivery != "release" || current.RecipeSHA256 != RecipeSHA256 || !m.Current().Available || m.Current().Prerequisites.Cargo {
					t.Errorf("pointer = %+v status = %+v", current, m.Current())
				}
				if m.CheckUpdate(context.Background()).UpdateAvailable {
					t.Error("same version reported as update")
				}
				if err := m.Update(context.Background(), func() {}); err != nil {
					t.Errorf("reinstall: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("update succeeded")
				}
				if data, _ := os.ReadFile(filepath.Join(managed, "current.json")); string(data) != old {
					t.Error("previous pointer changed")
				}
			}
			if m.Current().Updating {
				t.Error("still updating")
			}
			entries, _ := os.ReadDir(managed)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".download-") {
					t.Error("staging left behind")
				}
			}
		})
	}
}

func TestUpdateBlockedAndUnavailable(t *testing.T) {
	var fetched atomic.Bool
	m := newManager("/unused", func() bool { return true }, func(context.Context, string) (*http.Response, error) { fetched.Store(true); return respond(nil) })
	if err := m.Update(context.Background(), func() {}); err == nil || !strings.Contains(err.Error(), "while authentication") || fetched.Load() {
		t.Errorf("busy: %v", err)
	}
	root := t.TempDir()
	empty := newManager(root, func() bool { return false }, func(context.Context, string) (*http.Response, error) { return respond([]byte("[]")) })
	status := empty.CheckUpdate(context.Background())
	if status.ReleaseAvailable || status.BlockReason != "No compatible Nexus backend release has been published yet." {
		t.Errorf("status = %+v", status)
	}
	if err := empty.Update(context.Background(), func() {}); err == nil || !strings.Contains(err.Error(), "No compatible") {
		t.Errorf("update: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "xodus-backends", "current.json")); err == nil {
		t.Error("pointer created")
	}
}

func TestValidation(t *testing.T) {
	if _, err := ValidateManifest([]byte(`{"schema":1,"commit":"` + strings.Repeat("a", 40) + `"}`)); err == nil || !strings.Contains(err.Error(), "not a valid compatible release") {
		t.Errorf("manifest: %v", err)
	}
	if err := ValidateArchive([]string{PackageName(SourceLock.Version) + "/../../outside"}, []string{"-rw-r--r-- user/group 1 time outside"}, ""); err == nil || !strings.Contains(err.Error(), "Unsafe") {
		t.Errorf("archive: %v", err)
	}
}

func TestDiscover(t *testing.T) {
	newest := "backend-v9.9.9"
	name := func(version, suffix string) string {
		return "xodus-nexus-backend-" + version + "-linux-x86_64" + suffix
	}
	manifest := map[string]any{"schema": 1, "authUi": "electron", "version": "9.9.9", "commit": strings.Repeat("a", 40), "recipeSha256": strings.Repeat("b", 64),
		"compatibility": "linux-run-fixture", "platform": "linux", "arch": "x64", "archive": name("9.9.9", ".tar.xz"), "archiveSha512": strings.Repeat("c", 128),
		"files": map[string]any{"xodus-cli": strings.Repeat("d", 64), "xodus-service": strings.Repeat("e", 64), "LICENSE": strings.Repeat("f", 64)}}
	newestURL := func(asset string) string {
		return "https://github.com/" + Repository + "/releases/download/" + newest + "/" + asset
	}
	var assets []any
	for _, suffix := range []string{".tar.xz", ".manifest.json", ".sha512sum", "-source.tar.xz", "-source.sha512sum"} {
		assets = append(assets, map[string]any{"name": name("9.9.9", suffix), "browser_download_url": newestURL(name("9.9.9", suffix))})
	}
	fetch := func(ctx context.Context, target string) (*http.Response, error) {
		switch {
		case strings.Contains(target, "/releases?"):
			body, _ := json.Marshal([]any{map[string]any{"tag_name": "v9.9.9"},
				map[string]any{"tag_name": "backend-v" + SourceLock.Version, "assets": []any{map[string]any{"name": name(SourceLock.Version, ".tar.xz"),
					"browser_download_url": "https://github.com/" + Repository + "/releases/download/ignored/x"}}},
				map[string]any{"tag_name": "backend-v0.1.0"}, map[string]any{"tag_name": newest, "assets": assets}})
			return respond(body)
		case strings.HasSuffix(target, ".manifest.json"):
			body, _ := json.Marshal(manifest)
			return respond(body)
		case strings.HasSuffix(target, ".sha512sum"):
			return respond([]byte(manifest["archiveSha512"].(string) + "  " + manifest["archive"].(string) + "\n"))
		}
		return respond([]byte("bytes"))
	}
	artifact, err := Discover(context.Background(), fetch)
	if err != nil || artifact.Tag != newest || artifact.Manifest.Version != "9.9.9" {
		t.Errorf("artifact = %+v, %v", artifact, err)
	}
	old := func(context.Context, string) (*http.Response, error) {
		return respond([]byte(`[{"tag_name":"backend-v0.1.0","assets":[]},{"tag_name":"v0.1.0","assets":[]}]`))
	}
	if _, err := Discover(context.Background(), old); err == nil || !strings.Contains(err.Error(), "No compatible Nexus backend release") {
		t.Errorf("old: %v", err)
	}
	foreign := func(context.Context, string) (*http.Response, error) {
		body, _ := json.Marshal([]any{map[string]any{"tag_name": "backend-v" + SourceLock.Version, "assets": []any{
			map[string]any{"name": PackageName(SourceLock.Version) + ".manifest.json", "browser_download_url": "https://other.example/manifest"}}}})
		return respond(body)
	}
	if _, err := Discover(context.Background(), foreign); err == nil || !strings.Contains(err.Error(), "Unexpected") {
		t.Errorf("foreign: %v", err)
	}
}
