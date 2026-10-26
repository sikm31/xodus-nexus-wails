package updater

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var config = Config{Repository: "example/nexus", AssetName: func(version string) string { return "Xodus-Nexus-" + version + "-x86_64.AppImage" }}

func image(marker byte) []byte {
	data := bytes.Repeat([]byte{marker}, 100)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1})
	copy(data[8:], []byte{0x41, 0x49, 0x02})
	binary.LittleEndian.PutUint16(data[18:], 62)
	return data
}

func release(version string) Release {
	name := config.AssetName(version)
	base := "https://github.com/example/nexus/releases/download/v" + version + "/"
	return Release{TagName: "v" + version, Assets: []Asset{{Name: name, Size: 100, URL: base + name}, {Name: name + ".sha512sum", Size: 200, URL: base + name + ".sha512sum"}}}
}

func respond(body []byte) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func fixture(t *testing.T, bad string) (string, string, *Updater, Fetch) {
	root := t.TempDir()
	target := filepath.Join(root, "Launcher.AppImage")
	os.WriteFile(target, image(1), 0o700)
	next := image(2)
	if bad == "format" {
		next = make([]byte, 100)
	}
	r := release("0.2.3")
	fetch := func(_ context.Context, url string, _ http.Header) (*http.Response, error) {
		switch {
		case strings.Contains(url, "api.github"):
			data, _ := json.Marshal([]Release{r})
			return respond(data)
		case strings.HasSuffix(url, ".sha512sum"):
			sum := sha512.Sum512(next)
			digest := hex.EncodeToString(sum[:])
			if bad == "checksum" {
				digest = strings.Repeat("0", 128)
			}
			return respond([]byte(digest + "  " + r.Assets[0].Name + "\n"))
		}
		if bad == "network" {
			return nil, errors.New("Network interrupted")
		}
		return respond(next)
	}
	updater := New(config, "0.2.2", target, root, func() {}, fetch)
	updater.Initialize()
	updater.Check(context.Background())
	return root, target, updater, fetch
}

func TestSelectCandidate(t *testing.T) {
	backend := release("9.0.0")
	backend.TagName = "backend-v9.0.0"
	pre := release("8.0.0")
	pre.Prerelease = true
	if got := SelectCandidate(config, []Release{backend, pre, release("0.2.3"), release("0.2.4")}, "0.2.2"); got == nil || got.Version != "0.2.4" {
		t.Errorf("candidate = %+v", got)
	}
	if SelectCandidate(config, []Release{release("0.2.3")}, "0.2.3") != nil {
		t.Error("same version offered")
	}
	unsafe := release("0.2.3")
	unsafe.Assets[0].URL = "https://evil.example/update"
	if SelectCandidate(config, []Release{unsafe}, "0.2.2") != nil {
		t.Error("foreign URL accepted")
	}
}

func TestInstallAndRollback(t *testing.T) {
	root, target, updater, fetch := fixture(t, "")
	os.WriteFile(filepath.Join(root, "settings.json"), []byte("KEEP"), 0o644)
	if err := updater.Install(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); !bytes.Equal(data, image(2)) || !updater.State().RestartRequired || !updater.State().CanRollback {
		t.Error("install")
	}
	restarted := New(config, "0.2.3", target, root, func() {}, fetch)
	restarted.Initialize()
	if !restarted.State().CanRollback {
		t.Fatal("journal not restored")
	}
	if err := restarted.Rollback(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); !bytes.Equal(data, image(1)) {
		t.Error("rollback")
	}
	if data, _ := os.ReadFile(filepath.Join(root, "settings.json")); string(data) != "KEEP" {
		t.Error("settings touched")
	}
}

func TestFailuresPreserveImage(t *testing.T) {
	for _, bad := range []string{"checksum", "network", "format"} {
		root, target, updater, _ := fixture(t, bad)
		if err := updater.Install(); err == nil {
			t.Errorf("%s installed", bad)
		}
		if data, _ := os.ReadFile(target); !bytes.Equal(data, image(1)) {
			t.Errorf("%s changed the image", bad)
		}
		entries, _ := os.ReadDir(root)
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "nexus-update") || strings.Contains(entry.Name(), "nexus-backup") {
				t.Errorf("%s left %s", bad, entry.Name())
			}
		}
		if updater.State().Updating {
			t.Errorf("%s still updating", bad)
		}
	}
}

func TestUnsafeImageAndDisabledUpdates(t *testing.T) {
	root, target, _, fetch := fixture(t, "")
	os.Chmod(target, 0o777)
	updater := New(config, "0.2.2", target, root, func() {}, fetch)
	updater.Initialize()
	updater.Check(context.Background())
	if updater.State().CanInstall || !strings.Contains(updater.State().ReleaseURL, "/releases/tag/v0.2.3") {
		t.Errorf("state = %+v", updater.State())
	}
	disabled := New(Config{}, "0.2.2", target, root, func() {}, fetch)
	disabled.Initialize()
	disabled.Check(context.Background())
	if disabled.State().CanInstall || !strings.Contains(disabled.State().Error, "not configured") {
		t.Errorf("disabled = %+v", disabled.State())
	}
}
