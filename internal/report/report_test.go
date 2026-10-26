package report

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

func backendLog() (shared.DiagnosticLog, error) {
	return shared.DiagnosticLog{Name: "backend.log", Content: "operation=library status=401 code=1"}, nil
}

func base(root string) Input {
	return Input{Snapshot: shared.DemoSnapshot(shared.DemoVersions{}, nil), LauncherVersion: "0.3.10", SystemInfo: "Test system", BackendLog: backendLog, LogsRoot: root}
}

func TestRedact(t *testing.T) {
	report := Redact(strings.Join([]string{
		"HTTP 401 CONTENT_NOT_ENTITLED status=200",
		"Authorization: Bearer shortsecret", "Cookie: session=secretcookie", "refresh_token=private-refresh",
		"user_hash=12345", "accountId=private-id", "xuid: 1234567891234567",
		`{"username":"secret-user","displayName":"real-name"}`,
		"\x1b[31mAuthori\x1b[0mzation: hidden-auth",
		"https://example.test/package?unfamiliar=secret-signature", "https://example.test/?email_token=secret-notification",
		"contact person@example.com", "abcdefghijk.abcdefghijklmnop.abcdefghijk",
		"abcdefghijk..abcdefghijkl.abcdefghijkl.abcdefghijk", strings.Repeat("x", 120),
		"/home/secret-user/Games/Forza5/pfx", `C:\users\WindowsName\AppData`, "/run/user/1000/session",
		"Cool Gamer used /custom/private-home", "-----BEGIN RSA PRIVATE KEY-----\nprivate-key\n-----END RSA PRIVATE KEY-----",
	}, "\n"), []string{"Cool Gamer", "/custom/private-home"})
	for _, secret := range []string{"shortsecret", "secretcookie", "private-refresh", "12345", "private-id", "real-name", "hidden-auth", "secret-signature",
		"secret-notification", "person@example.com", "abcdefghijk", "secret-user", "WindowsName", "/1000/", "Cool Gamer", "private-home", "private-key", strings.Repeat("x", 100)} {
		if strings.Contains(report, secret) {
			t.Errorf("leaked %q", secret)
		}
	}
	if !strings.Contains(report, "HTTP 401 CONTENT_NOT_ENTITLED status=200") || !strings.Contains(report, "/home/[user]/Games/Forza5/pfx") {
		t.Errorf("report = %s", report)
	}
}

func TestReportContents(t *testing.T) {
	root := t.TempDir()
	input := base(root)
	input.Snapshot.Backend.Available, input.Snapshot.Backend.Version, input.Snapshot.Runtime.Name = false, "0.2.10", "runtime-test"
	input.Snapshot.Downloads = nil
	for i := range 7 {
		exit := 1
		input.Snapshot.Downloads = append(input.Snapshot.Downloads, shared.DownloadTask{ID: string(rune('0' + i)), Title: "Title " + string(rune('0'+i)),
			ProductID: "Product" + string(rune('0'+i)), Destination: "/secret/game", Status: "failed", Phase: "Downloading",
			Log: []string{"line " + string(rune('0'+i)), "access_token=private-download"}, StartedAt: "today", Error: "HTTP 503", ExitCode: &exit})
	}
	report, err := Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^nexus-diagnostics-.*\.txt$`).MatchString(report.Name) {
		t.Errorf("name = %s", report.Name)
	}
	for _, value := range []string{"Launcher: 0.3.10", "Backend: 0.2.10; available: false", "Runtime: runtime-test", "status=401", "Error: HTTP 503", "Title 0", "Title 4", "[No game selected]"} {
		if !strings.Contains(report.Content, value) {
			t.Errorf("missing %q", value)
		}
	}
	for _, value := range []string{"Title 5", "private-download", "/secret/game", input.Snapshot.Auth.Username} {
		if strings.Contains(report.Content, value) {
			t.Errorf("contains %q", value)
		}
	}
}

func gameInput(t *testing.T, root string) (Input, string) {
	input := base(root)
	id := "0123456789abcdef"
	input.Snapshot.Games = []shared.Game{input.Snapshot.Games[0]}
	input.Snapshot.Games[0].ID = id
	input.GameID = &id
	return input, id
}

func TestMissingLogs(t *testing.T) {
	input, _ := gameInput(t, t.TempDir())
	input.BackendLog = func() (shared.DiagnosticLog, error) { return shared.DiagnosticLog{}, errors.New("/secret/backend") }
	report, _ := Create(input)
	if strings.Count(report.Content, "Log missing") != 3 || strings.Contains(report.Content, "/secret/backend") {
		t.Errorf("report = %s", report.Content)
	}
}

func TestSelectedGameLogs(t *testing.T) {
	root := t.TempDir()
	input, id := gameInput(t, root)
	input.Snapshot.Games[0].Path = filepath.Join(root, "original-game")
	os.Mkdir(filepath.Join(root, "original-game"), 0o755)
	os.WriteFile(filepath.Join(root, "original-game", "secret"), []byte("do-not-read"), 0o644)
	os.WriteFile(filepath.Join(root, id+"-latest.log"), []byte("game failure 0xc0000005\nhttps://test/?sig=private\n"), 0o644)
	os.WriteFile(filepath.Join(root, id+"-vc-runtime.log"), []byte("VC exit 0"), 0o644)
	report, _ := Create(input)
	if !strings.Contains(report.Content, "game failure 0xc0000005") || !strings.Contains(report.Content, "VC exit 0") ||
		strings.Contains(report.Content, "do-not-read") || strings.Contains(report.Content, "?sig=private") {
		t.Errorf("report = %s", report.Content)
	}
	for _, kind := range []string{"symlink", "hardlink", "directory"} {
		root := t.TempDir()
		input, id := gameInput(t, root)
		target, secret := filepath.Join(root, id+"-latest.log"), filepath.Join(root, "not-a-log")
		os.WriteFile(secret, []byte("private-file-contents"), 0o644)
		switch kind {
		case "symlink":
			os.Symlink(secret, target)
		case "hardlink":
			os.Link(secret, target)
		case "directory":
			os.Mkdir(target, 0o755)
		}
		if report, _ := Create(input); strings.Contains(report.Content, "private-file-contents") || !strings.Contains(report.Content, "unavailable for safe reading") {
			t.Errorf("%s accepted", kind)
		}
	}
}

func TestBoundedLogs(t *testing.T) {
	root := t.TempDir()
	input, id := gameInput(t, root)
	os.WriteFile(filepath.Join(root, id+"-latest.log"), []byte("old header\n"+strings.Repeat("log line\n", 200_000)+"last failure 70\n"), 0o644)
	report, _ := Create(input)
	if !strings.Contains(report.Content, "[TRUNCATED") || !strings.Contains(report.Content, "last failure 70") || strings.Contains(report.Content, "old header") || len(report.Content) >= 2*1024*1024 {
		t.Error("game log not bounded")
	}
	backend := base(t.TempDir())
	backend.BackendLog = func() (shared.DiagnosticLog, error) {
		return shared.DiagnosticLog{Name: "backend.log", Content: "old header\n" + strings.Repeat("x", 2*1024*1024) + "\nHTTP 503\n"}, nil
	}
	report, _ = Create(backend)
	if !strings.Contains(report.Content, "[TRUNCATED") || !strings.Contains(report.Content, "HTTP 503") || strings.Contains(report.Content, "old header") || strings.Contains(report.Content, strings.Repeat("x", 100)) {
		t.Error("backend log not bounded")
	}
}

func TestGameValidation(t *testing.T) {
	input := base(t.TempDir())
	bad := "../private"
	input.GameID = &bad
	if _, err := Create(input); err == nil || !strings.Contains(err.Error(), "Game not found") {
		t.Errorf("traversal: %v", err)
	}
	input.Snapshot.Games[0].ID = bad
	if _, err := Create(input); err == nil || !strings.Contains(err.Error(), "Game not found") {
		t.Errorf("invalid ID: %v", err)
	}
}

func TestSave(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "report.txt")
	if err := Save(file, "edited report"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
		t.Error("report must be private")
	}
	if Save(file, "replaced") == nil {
		t.Error("overwrite allowed")
	}
	os.Symlink(file, filepath.Join(root, "link.txt"))
	if Save(filepath.Join(root, "link.txt"), "replaced") == nil {
		t.Error("followed a link")
	}
	for _, value := range []string{"", "\x00bad", strings.Repeat("x", ReportLimit+1)} {
		if err := Save(filepath.Join(root, "new.txt"), value); err == nil || !strings.Contains(err.Error(), "Invalid") {
			t.Errorf("content accepted: %v", err)
		}
	}
	if err := Save(filepath.Join(root, "report.html"), "bad"); err == nil || !strings.Contains(err.Error(), ".txt") {
		t.Errorf("html: %v", err)
	}
	if data, _ := os.ReadFile(file); string(data) != "edited report" {
		t.Error("report changed")
	}
}

func TestSystemInfo(t *testing.T) {
	root := t.TempDir()
	release, drm := filepath.Join(root, "os-release"), filepath.Join(root, "drm")
	os.WriteFile(release, []byte("PRETTY_NAME=\"Test Linux\"\nID=test\nVERSION_ID=42\nPRIVATE_SECRET=not-public\n"), 0o644)
	device := filepath.Join(drm, "card0", "device")
	os.MkdirAll(device, 0o755)
	os.WriteFile(filepath.Join(device, "vendor"), []byte("0x1002\n"), 0o644)
	os.WriteFile(filepath.Join(device, "device"), []byte("0x1234\n"), 0o644)
	os.WriteFile(filepath.Join(device, "serial"), []byte("serial-not-public"), 0o644)
	os.Symlink("/drivers/amdgpu", filepath.Join(device, "driver"))
	content := SystemInfo(launcherenv.Env{"XDG_CURRENT_DESKTOP": "gamescope:KDE:private-session", "XDG_SESSION_TYPE": "wayland",
		"SteamGameId": "private-steam-id", "SECRET": "env-not-public"}, Roots{OSRelease: release, DRM: drm})
	for _, value := range []string{"PRETTY_NAME: Test Linux", "Desktop: gamescope:kde", "Session: wayland", "Steam launch: true", "0x1002:0x1234, driver amdgpu", "CPU:", "RAM:"} {
		if !strings.Contains(content, value) {
			t.Errorf("missing %q in %s", value, content)
		}
	}
	for _, value := range []string{"not-public", "private-session", "private-steam-id", "SECRET"} {
		if strings.Contains(content, value) {
			t.Errorf("leaked %q", value)
		}
	}
}
