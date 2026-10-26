package protonruntime

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func respond(status int, body []byte) (*http.Response, error) {
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func releases(value any) Fetch {
	body, _ := json.Marshal(value)
	return func(context.Context, string, http.Header) (*http.Response, error) { return respond(200, body) }
}

func pair(name string) []any {
	var out []any
	for _, file := range []string{name, strings.TrimSuffix(name, ".tar.xz") + ".sha512sum"} {
		out = append(out, map[string]any{"name": file, "browser_download_url": "https://example.com/" + file})
	}
	return out
}

func manager(root string, busy bool, fetch Fetch) *Manager {
	return NewManager(root, func(string, string) {}, func() bool { return busy }, fetch)
}

func TestNames(t *testing.T) {
	if NexusReleaseName("xodus-nexus-runtime-11.0-20261011-g3fd4ae10-x86_64.tar.xz") != "xodus-nexus-runtime-11.0-20261011-g3fd4ae10" ||
		CanonicalSnapshotName("3fd4ae1091339ef07d6f2d53fda8e15541f96ad3") != "xodus-proton-3fd4ae10" {
		t.Error("release names")
	}
	for _, name := range []string{"", "-bridge-v1", "-bridge-v1-userstate-v1", "-bridge-v1-userstate-v1-userguest-v1", "-bridge-v1-userstate-v1-userguest-v1-locale-v1", "-locale-v1"} {
		if got := PreviewBaseRelease("proton-xodus-test-offline-v2-xgamesave-pr28-3fd4ae1091" + name); got != "proton-xodus-test-offline-v2" {
			t.Errorf("%s = %q", name, got)
		}
	}
	if PreviewBaseRelease("proton-xodus-test-offline-v2") != "" || PreviewBaseRelease("../escape-xgamesave-pr28-3fd4ae1091") != "" {
		t.Error("malformed names accepted")
	}
}

func TestCheckNexus(t *testing.T) {
	name := "xodus-nexus-runtime-11.0-20261011-g3fd4ae10-x86_64.tar.xz"
	status := manager("/tmp/unused", false, releases([]any{
		map[string]any{"tag_name": "v0.5.25", "assets": []any{map[string]any{"name": "Xodus-Nexus-0.5.25-x86_64.AppImage", "browser_download_url": "https://example.com/app"}}},
		map[string]any{"tag_name": "runtime-11.0-20261011-g3fd4ae10", "assets": pair(name)},
	})).CheckNexus(context.Background())
	if !status.NexusAvailable || status.NexusVersion != "runtime-11.0-20261011-g3fd4ae10" || status.NexusName != name {
		t.Errorf("status = %+v", status)
	}
	runtime := func(suffix, published string) map[string]any {
		return map[string]any{"tag_name": "runtime-11.0-20261014-" + suffix, "published_at": published, "assets": pair("xodus-nexus-runtime-11.0-20261014-" + suffix + "-x86_64.tar.xz")}
	}
	for _, reversed := range []bool{false, true} {
		older, newer := runtime("g9e1653e1", "2026-10-14T12:18:00Z"), runtime("g606bbea0", "2026-10-14T22:13:42Z")
		ordered := []any{older, newer}
		if reversed {
			ordered = []any{newer, older}
		}
		draft, preview, incomplete := runtime("draft", "2026-10-16T00:00:00Z"), runtime("preview", "2026-10-16T00:00:00Z"), runtime("incomplete", "2026-10-16T00:00:00Z")
		draft["draft"], preview["prerelease"] = true, true
		incomplete["assets"] = older["assets"].([]any)[:1]
		list := append([]any{map[string]any{"tag_name": "v0.2.3", "published_at": "2026-10-15T00:00:00Z", "assets": []any{}}}, ordered...)
		list = append(list, draft, preview, incomplete, runtime("undated", "invalid"))
		if status := manager("/tmp/unused", false, releases(list)).CheckNexus(context.Background()); status.NexusVersion != newer["tag_name"] {
			t.Errorf("reversed=%v version = %s", reversed, status.NexusVersion)
		}
	}
	none := manager("/tmp/unused", false, releases([]any{map[string]any{"tag_name": "v0.5.24", "assets": []any{}}})).CheckNexus(context.Background())
	if none.NexusAvailable || !strings.Contains(none.NexusError, "No Nexus runtime release has been published yet") {
		t.Errorf("none = %+v", none)
	}
	tested := "xodus-nexus-runtime-11.0-tested-x86_64.tar.xz"
	asset := func(file string) any {
		return map[string]any{"name": file, "browser_download_url": "https://example.com/" + file}
	}
	mixed := manager("/tmp/unused", false, releases([]any{
		map[string]any{"tag_name": "draft", "draft": true, "assets": pair(tested)},
		map[string]any{"tag_name": "preview", "prerelease": true, "assets": pair(tested)},
		map[string]any{"tag_name": "unrelated", "assets": []any{asset("other-x86_64.tar.xz"), asset("other-x86_64.sha512sum")}},
		map[string]any{"tag_name": "mismatched", "assets": []any{asset(tested), asset("xodus-nexus-runtime-different-x86_64.sha512sum")}},
		map[string]any{"tag_name": "runtime-tested", "assets": pair(tested)},
	})).CheckNexus(context.Background())
	if mixed.NexusVersion != "runtime-tested" {
		t.Errorf("mixed = %s", mixed.NexusVersion)
	}
}

func TestForkFallback(t *testing.T) {
	old, next := "xodus-nexus-runtime-tested-x86_64.tar.xz", "nexus-runtime-tested-x86_64.tar.xz"
	for _, scenario := range []string{"empty", "404", "preview", "incomplete"} {
		var calls []string
		fetch := func(_ context.Context, url string, _ http.Header) (*http.Response, error) {
			calls = append(calls, url)
			if strings.Contains(url, "/nexus-runtime/") {
				if scenario == "404" {
					return respond(404, nil)
				}
				assets := pair(next)
				if scenario == "incomplete" {
					assets = assets[:1]
				}
				list := []any{map[string]any{"tag_name": "next", "prerelease": scenario == "preview", "assets": assets}}
				if scenario == "empty" {
					list = []any{}
				}
				body, _ := json.Marshal(list)
				return respond(200, body)
			}
			body, _ := json.Marshal([]any{map[string]any{"tag_name": "previous", "assets": pair(old)}})
			return respond(200, body)
		}
		if status := manager("/tmp/unused", false, fetch).CheckNexus(context.Background()); status.NexusName != old || len(calls) != 2 {
			t.Errorf("%s: %s after %d calls", scenario, status.NexusName, len(calls))
		}
	}
	root := t.TempDir()
	previous := `{"name":"previous","winePath":"/previous/wine","commit":"previous","installedAt":"before"}`
	os.WriteFile(filepath.Join(root, "current.json"), []byte(previous), 0o644)
	fork := "nexus-runtime-11.0-experimental-g12345678-x86_64.tar.xz"
	calls := 0
	status := manager(root, false, func(_ context.Context, url string, _ http.Header) (*http.Response, error) {
		calls++
		if !strings.Contains(url, "/domsvoboda/nexus-runtime/releases?") {
			t.Errorf("unexpected %s", url)
		}
		body, _ := json.Marshal([]any{map[string]any{"tag_name": "next", "assets": pair(fork)}})
		return respond(200, body)
	}).CheckNexus(context.Background())
	if data, _ := os.ReadFile(filepath.Join(root, "current.json")); status.NexusName != fork || calls != 1 || string(data) != previous {
		t.Errorf("fork = %+v", status)
	}
	for _, code := range []int{403, 500} {
		calls := 0
		status := manager("/tmp/unused", false, func(context.Context, string, http.Header) (*http.Response, error) { calls++; return respond(code, nil) }).CheckNexus(context.Background())
		if status.NexusAvailable || !strings.Contains(status.NexusError, "could not be read") || calls != 1 {
			t.Errorf("%d: %+v", code, status)
		}
	}
}

func TestInstallBlockedWhileBusy(t *testing.T) {
	m := manager("/tmp/unused", true, func(context.Context, string, http.Header) (*http.Response, error) {
		return nil, errors.New("No download may start")
	})
	if _, err := m.InstallNexus(context.Background(), func() {}); err == nil || !strings.Contains(err.Error(), "while a Xodus operation is active") {
		t.Errorf("busy: %v", err)
	}
}

func TestInstallNexus(t *testing.T) {
	for _, scenario := range []string{"valid", "new-fork", "stderr-warning", "bad-digest", "wrong-file", "unsafe-root", "mixed-root"} {
		t.Run(scenario, func(t *testing.T) {
			release := "xodus-nexus-runtime-11.0-20261011-g3fd4ae10"
			if scenario == "new-fork" {
				release = "nexus-runtime-11.0-experimental-g3fd4ae10"
			}
			root := t.TempDir()
			build := filepath.Join(root, "build")
			top := "proton-xodus-bleeding-edge-11.0-20260803-3-g7c0b4354-x86_64"
			switch scenario {
			case "new-fork":
				top = release
			case "unsafe-root":
				top = "unexpected-root"
			}
			wine := filepath.Join(build, top, "files", "bin", "wine")
			os.MkdirAll(filepath.Dir(wine), 0o755)
			os.WriteFile(wine, []byte("#!/bin/sh\necho wine-11.0-test\n"), 0o755)
			members := []string{top}
			if scenario == "mixed-root" {
				os.Mkdir(filepath.Join(build, "second-root"), 0o755)
				members = append(members, "second-root")
			}
			archive := release + "-x86_64.tar.xz"
			if output, err := exec.Command("tar", append([]string{"-cJf", filepath.Join(build, archive), "-C", build}, members...)...).CombinedOutput(); err != nil {
				t.Fatalf("tar: %v %s", err, output)
			}
			data, _ := os.ReadFile(filepath.Join(build, archive))
			sum := sha512.Sum512(data)
			digest := hex.EncodeToString(sum[:])
			if scenario == "bad-digest" {
				digest = strings.Repeat("0", 128)
			}
			named := archive
			if scenario == "wrong-file" {
				named = "other.tar.xz"
			}
			os.WriteFile(filepath.Join(build, release+"-x86_64.sha512sum"), []byte(digest+"  "+named+"\n"), 0o644)
			fetch := func(_ context.Context, url string, _ http.Header) (*http.Response, error) {
				if strings.Contains(url, "/releases?") {
					body, _ := json.Marshal([]any{map[string]any{"tag_name": "runtime-11.0-20261011-g3fd4ae10", "assets": pair(archive)}})
					return respond(200, body)
				}
				data, err := os.ReadFile(filepath.Join(build, filepath.Base(url)))
				if err != nil {
					return respond(404, nil)
				}
				return respond(200, data)
			}
			if scenario == "stderr-warning" {
				t.Setenv("LD_PRELOAD", filepath.Join(root, "nonexistent-test-overlay.so"))
			}
			previousWine := filepath.Join(root, "releases", "previous", "files", "bin", "wine")
			os.MkdirAll(filepath.Dir(previousWine), 0o755)
			os.WriteFile(previousWine, []byte("#!/bin/sh\necho wine-previous\n"), 0o755)
			previous, _ := json.Marshal(map[string]any{"name": "previous", "winePath": previousWine, "commit": "previous", "installedAt": "before"})
			os.WriteFile(filepath.Join(root, "current.json"), previous, 0o644)
			m := manager(root, false, fetch)
			m.commandOK = func(command string, args ...string) bool { return command == "tar" || command == "sha512sum" }
			m.Refresh()
			winePath, err := m.InstallNexus(context.Background(), func() {})
			if scenario == "valid" || scenario == "new-fork" || scenario == "stderr-warning" {
				var current pointer
				data, _ := os.ReadFile(filepath.Join(root, "current.json"))
				json.Unmarshal(data, &current)
				if err != nil || winePath != filepath.Join(root, "releases", release, "files", "bin", "wine") || current.Name != release ||
					current.Commit != "runtime-11.0-20261011-g3fd4ae10" || !m.Current().Installed || m.Current().Name != release {
					t.Errorf("install = %q, %v, pointer %+v", winePath, err, current)
				}
				if data, _ := os.ReadFile(previousWine); !strings.Contains(string(data), "wine-previous") {
					t.Error("previous release modified")
				}
				return
			}
			want := "SHA-512"
			if scenario == "unsafe-root" || scenario == "mixed-root" {
				want = "runtime structure"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want %q", err, want)
			}
			if data, _ := os.ReadFile(filepath.Join(root, "current.json")); string(data) != string(previous) || m.Current().Name != "previous" || m.Current().Updating {
				t.Error("previous runtime must stay active")
			}
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".staging-") {
					t.Error("staging left behind")
				}
			}
		})
	}
}

func TestRollbackPreview(t *testing.T) {
	root := t.TempDir()
	base := "proton-xodus-test-offline-v2"
	winePath := filepath.Join(root, "releases", base, "files", "bin", "wine")
	os.MkdirAll(filepath.Dir(winePath), 0o755)
	os.WriteFile(winePath, []byte("#!/bin/sh\necho wine-test\n"), 0o755)
	current, _ := json.Marshal(map[string]any{"name": base + "-xgamesave-pr28-3fd4ae1091", "winePath": "/preview/wine", "commit": "abc", "installedAt": "before"})
	os.WriteFile(filepath.Join(root, "current.json"), current, 0o644)
	got, err := manager(root, false, nil).RollbackCloudSavePreview(func() {})
	var restored pointer
	data, _ := os.ReadFile(filepath.Join(root, "current.json"))
	json.Unmarshal(data, &restored)
	if err != nil || got != winePath || restored.Name != base || restored.WinePath != winePath {
		t.Errorf("rollback = %q, %v, %+v", got, err, restored)
	}
}
