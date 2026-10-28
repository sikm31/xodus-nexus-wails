package eaapp

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeProton(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "files", "share", "default_pfx"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proton"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestFindProtonPreference(t *testing.T) {
	steam := t.TempDir()
	xodus := filepath.Join(t.TempDir(), "xodus-proton-1")
	fakeProton(t, xodus)
	wine := filepath.Join(xodus, "files", "bin", "wine")
	if proton, ok := FindProton(steam, wine); !ok || proton.Root != xodus {
		t.Fatalf("fallback = %+v %v", proton, ok)
	}
	fakeProton(t, filepath.Join(steam, "steamapps", "common", "Proton 8.0"))
	fakeProton(t, filepath.Join(steam, "steamapps", "common", "Proton 9.0"))
	if proton, _ := FindProton(steam, wine); proton.Name != "Proton 9.0" {
		t.Errorf("numbered = %+v", proton)
	}
	fakeProton(t, filepath.Join(steam, "steamapps", "common", "Proton - Experimental"))
	if proton, _ := FindProton(steam, wine); proton.Name != "Proton - Experimental" {
		t.Errorf("experimental = %+v", proton)
	}
	for _, name := range []string{"GE-Proton9-27", "GE-Proton10-25", "GE-Proton10-3"} {
		fakeProton(t, filepath.Join(steam, "compatibilitytools.d", name))
	}
	// An unusable newer build is skipped.
	os.MkdirAll(filepath.Join(steam, "compatibilitytools.d", "GE-Proton11-1"), 0o755)
	if proton, _ := FindProton(steam, wine); proton.Name != "GE-Proton10-25" {
		t.Errorf("ge = %+v", proton)
	}
}

func TestExecutable(t *testing.T) {
	paths := Paths{Root: t.TempDir()}
	if paths.Executable() != "" || paths.running() {
		t.Fatal("empty prefix reported as installed or running")
	}
	root := filepath.Join(paths.Prefix(), "drive_c/Program Files/Electronic Arts/EA Desktop")
	for _, version := range []string{"13.9.0.100", "13.813.1.6327", "13.10.2.1"} {
		exe := filepath.Join(root, version, "EA Desktop", "EADesktop.exe")
		os.MkdirAll(filepath.Dir(exe), 0o755)
		os.WriteFile(exe, nil, 0o644)
	}
	// A leftover version directory without the executable is skipped.
	os.MkdirAll(filepath.Join(root, "14.0.0.1", "EA Desktop"), 0o755)
	if want := filepath.Join(root, "13.813.1.6327", "EA Desktop", "EADesktop.exe"); paths.Executable() != want {
		t.Errorf("executable = %q", paths.Executable())
	}
}

func TestDownloadRejectsNonExecutable(t *testing.T) {
	serve := func(status int, body string) Fetch {
		return func(context.Context, string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	}
	target := filepath.Join(t.TempDir(), "EAappInstaller.exe")
	for _, fetch := range []Fetch{serve(200, "<html>"), serve(404, "MZ")} {
		m := NewManager(func() Paths { return Paths{} }, func() string { return "" }, func() {}, fetch)
		if m.download(context.Background(), target) == nil {
			t.Error("invalid download accepted")
		}
	}
	m := NewManager(func() Paths { return Paths{} }, func() string { return "" }, func() {}, serve(200, "MZ\x90\x00"))
	if err := m.download(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "MZ\x90\x00" {
		t.Errorf("installer = %q", data)
	}
}

func TestSignOutRemovesSessionOnly(t *testing.T) {
	paths := Paths{Root: t.TempDir()}
	session := filepath.Join(paths.Prefix(), "drive_c/users/steamuser/AppData/Local/Electronic Arts/EA Desktop")
	game := filepath.Join(paths.Prefix(), "drive_c/Program Files/EA Games/Dead Space 2/game.exe")
	for _, file := range []string{filepath.Join(session, "user_123.ini"), game} {
		os.MkdirAll(filepath.Dir(file), 0o755)
		os.WriteFile(file, nil, 0o644)
	}
	if !paths.signedIn() {
		t.Fatal("session not detected")
	}
	m := NewManager(func() Paths { return paths }, func() string { return "" }, func() {}, nil)
	if err := m.SignOut(); err != nil {
		t.Fatal(err)
	}
	if paths.signedIn() {
		t.Error("session still present")
	}
	if _, err := os.Stat(game); err != nil {
		t.Error("installed game was removed")
	}
}
