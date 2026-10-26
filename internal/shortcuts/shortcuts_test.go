package shortcuts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const id = "0123456789abcdef"

var game = Game{ID: id, Title: "A game\nExec=unwanted", InstallState: "ready"}

type fixture struct {
	root      string
	locations Locations
	shortcuts *Shortcuts
}

func newFixture(t *testing.T) fixture {
	root := t.TempDir()
	launcher := filepath.Join(root, "Launcher $`%' name.AppImage")
	echo, _ := os.ReadFile("/bin/echo")
	os.WriteFile(launcher, echo, 0o700)
	locations := Locations{UserData: filepath.Join(root, "profile"), Desktop: filepath.Join(root, "Desktop"),
		Applications: filepath.Join(root, "applications"), Launcher: launcher, Icon: []byte("fixture icon")}
	return fixture{root: root, locations: locations, shortcuts: New(locations)}
}

func (f fixture) init(t *testing.T) {
	if err := f.shortcuts.Initialize(); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, command string, args ...string) string {
	out, err := exec.Command(command, args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func read(t *testing.T, file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mode(file string) os.FileMode {
	info, _ := os.Stat(file)
	return info.Mode().Perm()
}

func TestSteamDesktop(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	_, file, err := f.shortcuts.SteamDesktop(&game, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := read(t, file)
	if file != filepath.Join(f.locations.UserData, "shortcuts/steam", id+".desktop") || strings.Count(body, "\nExec=") != 1 ||
		!strings.Contains(body, "Name=A game Exec=unwanted\n") || !strings.Contains(body, DesktopArgument(filepath.Join(f.locations.UserData, "shortcuts/launch-game"))) ||
		!strings.Contains(body, DesktopArgument(id)) || mode(file) != 0o600 {
		t.Errorf("body = %q", body)
	}
	if _, err := os.Stat(f.locations.Applications); err == nil {
		t.Error("application menu touched")
	}
	_, nexus, _ := f.shortcuts.SteamDesktop(nil, nil)
	if !strings.Contains(read(t, nexus), "Name=Xodus Nexus\n") {
		t.Error("launcher entry")
	}
	os.WriteFile(file, []byte("unrelated user file"), 0o600)
	if _, _, err := f.shortcuts.SteamDesktop(&game, nil); err == nil || !strings.Contains(err.Error(), "unrelated") || read(t, file) != "unrelated user file" {
		t.Errorf("unrelated: %v", err)
	}
}

func TestSteamLaunch(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	named := game
	named.Title = "A game"
	launch, err := f.shortcuts.SteamLaunch(&named, nil)
	if err != nil || launch.Executable != `"/bin/sh"` || !strings.Contains(launch.Options, `"`+id+`"`) {
		t.Fatalf("launch = %+v, %v", launch, err)
	}
	if got := run(t, "/bin/sh", filepath.Join(f.locations.UserData, "shortcuts/launch-game"), id); got != "--nexus-profile "+f.locations.UserData+" --launch-game "+id {
		t.Errorf("launch-game = %q", got)
	}
	nexus, _ := f.shortcuts.SteamLaunch(nil, nil)
	if nexus.Title != "Xodus Nexus" || nexus.Identity == launch.Identity {
		t.Error("launcher identity")
	}
	if got := run(t, "/bin/sh", filepath.Join(f.locations.UserData, "shortcuts/launch-nexus")); got != "--nexus-profile "+f.locations.UserData {
		t.Errorf("launch-nexus = %q", got)
	}
	for _, path := range []string{f.locations.Desktop, f.locations.Applications} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s created", path)
		}
	}
	for _, bad := range []Game{{ID: id, Title: "x", InstallState: "imported"}, {ID: "../bad", Title: "x", InstallState: "ready"}} {
		if _, err := f.shortcuts.SteamLaunch(&bad, nil); err == nil || !strings.Contains(err.Error(), "ready") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestCreateEntries(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	target, err := f.shortcuts.Create(game, "desktop", nil)
	if err != nil {
		t.Fatal(err)
	}
	menu, _ := f.shortcuts.Create(game, "applications", nil)
	body := read(t, target)
	if !strings.Contains(body, "Name=A game Exec=unwanted\n") || strings.Count(body, "\nExec=") != 1 || !strings.Contains(body, "Exec=/bin/sh ") ||
		mode(target) != 0o755 || mode(menu) != 0o644 {
		t.Errorf("entry = %q", body)
	}
	entry := filepath.Join(f.locations.UserData, "shortcuts/launch-game")
	want := "--nexus-profile " + f.locations.UserData + " --launch-game " + id
	if run(t, entry, id) != want {
		t.Error("entry runs launcher")
	}
	before := read(t, target)
	os.Rename(f.locations.Launcher, f.locations.Launcher+".old")
	data, _ := os.ReadFile(f.locations.Launcher + ".old")
	os.WriteFile(f.locations.Launcher, data, 0o700)
	if run(t, entry, id) != want {
		t.Error("AppImage update broke the shortcut")
	}
	moved := filepath.Join(f.root, "Moved launcher.AppImage")
	os.Rename(f.locations.Launcher, moved)
	relocated := f.locations
	relocated.Launcher = moved
	if err := New(relocated).Initialize(); err != nil {
		t.Fatal(err)
	}
	if run(t, entry, id) != want || read(t, target) != before || strings.Contains(read(t, entry), f.locations.Launcher+"'") {
		t.Error("moved launcher")
	}
	f.shortcuts.Remove(id)
	for _, file := range []string{target, menu} {
		if _, err := os.Stat(file); err == nil {
			t.Errorf("%s not removed", file)
		}
	}
}

func TestCreateRejections(t *testing.T) {
	f := newFixture(t)
	unsupported := f.locations
	unsupported.Launcher = ""
	plain := New(unsupported)
	plain.Initialize()
	if _, err := plain.Create(game, "desktop", nil); plain.Available() || err == nil || !strings.Contains(err.Error(), "installed Linux launcher") {
		t.Errorf("unsupported: %v", err)
	}
	f.init(t)
	for _, c := range []struct {
		game        Game
		destination string
		want        string
	}{{Game{ID: "../bad", Title: "x", InstallState: "ready"}, "desktop", "Invalid game"}, {Game{ID: id, Title: "x", InstallState: "imported"}, "desktop", "ready"},
		{game, "../elsewhere", "destination"}} {
		if _, err := f.shortcuts.Create(c.game, c.destination, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	target, _ := f.shortcuts.Create(game, "desktop", nil)
	os.WriteFile(target, []byte("unrelated user file"), 0o755)
	if collision, _ := f.shortcuts.Create(game, "desktop", nil); collision == target || read(t, target) != "unrelated user file" {
		t.Error("unrelated file overwritten")
	}
	f.shortcuts.Remove(id)
	if read(t, target) != "unrelated user file" {
		t.Error("unrelated file removed")
	}
	os.Remove(target)
	os.Symlink(filepath.Join(f.locations.UserData, "shortcuts/nexus.png"), target)
	if created, _ := f.shortcuts.Create(game, "desktop", nil); created == target {
		t.Error("followed a symlink")
	}
	os.MkdirAll(f.locations.Applications, 0o755)
	os.Chmod(f.locations.Applications, 0o777)
	if _, err := f.shortcuts.Create(game, "applications", nil); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Errorf("world-writable: %v", err)
	}
}

func TestTitledFiles(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	named := game
	named.Title = "Shift At Midnight"
	first, _ := f.shortcuts.Create(named, "desktop", nil)
	if filepath.Base(first) != "Shift At Midnight.desktop" {
		t.Errorf("first = %s", first)
	}
	if again, _ := f.shortcuts.Create(named, "desktop", nil); again != first {
		t.Error("recreate must reuse the file")
	}
	other := named
	other.ID = "fedcba9876543210"
	second, _ := f.shortcuts.Create(other, "desktop", nil)
	if filepath.Base(second) != "Shift At Midnight (fedcba98).desktop" || !strings.Contains(read(t, first), "X-Xodus-Nexus-Game="+id+"\n") {
		t.Errorf("second = %s", second)
	}
	profile := f.locations
	profile.UserData = filepath.Join(f.root, "other-profile")
	another := New(profile)
	another.Initialize()
	third, _ := another.Create(named, "desktop", nil)
	if third == first || third == second {
		t.Error("profile collision")
	}
	f.shortcuts.Remove(id)
	if _, err := os.Stat(first); err == nil {
		t.Error("first kept")
	}
	for _, file := range []string{second, third} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("%s removed", file)
		}
	}
}

func TestLegacyMigration(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	named := game
	named.Title = "Old title"
	first, _ := f.shortcuts.Create(named, "desktop", nil)
	absolute, _ := filepath.Abs(f.locations.UserData)
	sum := sha256.Sum256([]byte(absolute))
	legacy := filepath.Join(f.locations.Desktop, "xodus-nexus-"+string([]byte(hexString(sum[:]))[:12])+"-"+id+".desktop")
	os.Rename(first, legacy)
	os.WriteFile(legacy, []byte(strings.Replace(read(t, legacy), "X-Xodus-Nexus-Game="+id+"\n", "", 1)), 0o755)
	named.Title = "Shift At Midnight"
	renamed, _ := f.shortcuts.Create(named, "desktop", nil)
	if filepath.Base(renamed) != "Shift At Midnight.desktop" {
		t.Errorf("renamed = %s", renamed)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Error("legacy kept")
	}
	named.Title = "New title"
	newer, _ := f.shortcuts.Create(named, "desktop", nil)
	if _, err := os.Stat(renamed); err == nil {
		t.Error("old title kept")
	}
	f.shortcuts.Remove(id)
	if _, err := os.Stat(newer); err == nil {
		t.Error("newer kept")
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2], out[i*2+1] = digits[v>>4], digits[v&15]
	}
	return string(out)
}

func TestTitleStemAndArguments(t *testing.T) {
	for input, want := range map[string]string{"Shift At Midnight": "Shift At Midnight", "../../Game: A/B\\C?\n": "Game A B C", "../\x00": "Xodus Nexus game",
		"Příliš žluťoučký kůň": "Příliš žluťoučký kůň"} {
		if got := TitleStem(input); got != want {
			t.Errorf("TitleStem(%q) = %q", input, got)
		}
	}
	for _, title := range []string{strings.Repeat("A", 1000), strings.Repeat("猫", 1000), strings.Repeat("é", 1000)} {
		if stem := TitleStem(title); len(stem) > 120 || !utf8.ValidString(stem) {
			t.Errorf("stem too long: %d", len(stem))
		}
	}
	if request, _ := Arguments([]string{"launcher", "--nexus-profile", "/a profile", "--launch-game", id}); request != (Request{GameID: id, Profile: "/a profile"}) {
		t.Errorf("request = %+v", request)
	}
	if request, _ := Arguments([]string{"launcher", "--unrelated"}); request != (Request{}) {
		t.Error("unrelated flags")
	}
	for _, argv := range [][]string{{"--launch-game"}, {"--launch-game", "../game"}, {"--launch-game", id, "--launch-game", id}, {"--nexus-profile", "relative"}, {"--nexus-profile", "/x\nExec=evil"}} {
		if _, err := Arguments(argv); err == nil {
			t.Errorf("%q accepted", argv)
		}
	}
	if got := DesktopArgument("/a b/100%/\"$`\\"); got != `"/a b/100%%/\\"\\$\\`+"`"+`\\\\"` {
		t.Errorf("DesktopArgument = %s", got)
	}
	if _, err := InstanceRequest([]string{"--nexus-profile", "/tmp/profile", "--launch-game", id}, "/tmp/another-profile"); err == nil || !strings.Contains(err.Error(), "different Nexus profile") {
		t.Errorf("profile binding: %v", err)
	}
	if request, err := InstanceRequest([]string{"--launch-game", id}, "/tmp/profile"); err != nil || request.GameID != id {
		t.Error("game-only request")
	}
}

func TestConcurrentTitles(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	var group sync.WaitGroup
	paths := make([]string, 2)
	for i, gameID := range []string{id, "fedcba9876543210"} {
		group.Add(1)
		go func() {
			defer group.Done()
			paths[i], _ = f.shortcuts.Create(Game{ID: gameID, Title: "Same title", InstallState: "ready"}, "desktop", nil)
		}()
	}
	group.Wait()
	if paths[0] == paths[1] || !strings.Contains(read(t, paths[0]), "X-Xodus-Nexus-Game="+id) || !strings.Contains(read(t, paths[1]), "X-Xodus-Nexus-Game=fedcba9876543210") {
		t.Errorf("paths = %v", paths)
	}
}

var pngBytes, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII=")

func TestGameIcons(t *testing.T) {
	f := newFixture(t)
	f.init(t)
	target, _ := f.shortcuts.Create(game, "desktop", pngBytes)
	icon := filepath.Join(f.locations.UserData, "shortcuts/icons", id+".png")
	if !strings.Contains(read(t, target), "Icon="+icon+"\n") || read(t, icon) != string(pngBytes) || mode(icon) != 0o600 {
		t.Error("game icon")
	}
	menu, _ := f.shortcuts.Create(game, "applications", nil)
	if !strings.Contains(read(t, menu), "Icon="+icon+"\n") {
		t.Error("cached icon not reused")
	}
	other := game
	other.ID = "fedcba9876543210"
	fallback, _ := f.shortcuts.Create(other, "desktop", []byte("not an image"))
	if !strings.Contains(read(t, fallback), "Icon="+filepath.Join(f.locations.UserData, "shortcuts/nexus.png")+"\n") {
		t.Error("Nexus fallback")
	}
}

func TestShortcutLaunch(t *testing.T) {
	var mu sync.Mutex
	active := ""
	var calls []string
	finish := make(chan struct{})
	router := NewLaunch(func() string { mu.Lock(); defer mu.Unlock(); return active }, func(value string) error {
		mu.Lock()
		calls = append(calls, value)
		mu.Unlock()
		<-finish
		mu.Lock()
		active = value
		mu.Unlock()
		return nil
	})
	done := make(chan error)
	go func() { done <- router.Request(id) }()
	for !router.Preparing() {
	}
	if err := router.Request(id); err != nil {
		t.Error("duplicate must coalesce")
	}
	if err := router.Request("fedcba9876543210"); err == nil || !strings.Contains(err.Error(), "Another game") {
		t.Errorf("concurrent: %v", err)
	}
	close(finish)
	<-done
	router.Request(id)
	if len(calls) != 1 {
		t.Errorf("calls = %v", calls)
	}
	if err := router.Request("fedcba9876543210"); err == nil {
		t.Error("running game must block another")
	}
	failure := NewLaunch(func() string { return "" }, func(string) error { return errors.New("missing runtime") })
	if err := failure.Request(id); err == nil || failure.Preparing() {
		t.Error("failure recovery")
	}
}

func respond(status int, body []byte, header http.Header) IconFetch {
	return func(context.Context, string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), Header: header}, nil
	}
}

func TestLoadIcon(t *testing.T) {
	identity := func(b []byte) []byte { return b }
	data := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	requests := 0
	fetch := func(ctx context.Context, url string) (*http.Response, error) {
		requests++
		return respond(200, pngBytes, http.Header{})(ctx, url)
	}
	if !bytes.Equal(LoadIcon(data, "https://store-images.s-microsoft.com/game", identity, fetch), pngBytes) || requests != 0 {
		t.Error("local artwork preferred")
	}
	if !bytes.Equal(LoadIcon("data:image/png;base64,AAAA", "https://store-images.s-microsoft.com/game", identity, fetch), pngBytes) || requests != 1 {
		t.Error("remote fallback")
	}
	offline := func(context.Context, string) (*http.Response, error) { requests++; return nil, errors.New("offline") }
	requests = 0
	for _, url := range []string{"http://store-images.s-microsoft.com/x", "https://localhost/x", "https://store-images.s-microsoft.com.evil.test/x",
		"https://user:pass@store-images.microsoft.com/x", "file:///tmp/x"} {
		if LoadIcon("", url, identity, offline) != nil {
			t.Errorf("%s accepted", url)
		}
	}
	if requests != 0 {
		t.Error("untrusted URL fetched")
	}
	if LoadIcon(data, "", func([]byte) []byte { panic("bad image") }, nil) != nil {
		t.Error("decoder failure")
	}
	cover := "https://store-images.microsoft.com/game"
	if LoadIcon("", cover, identity, offline) != nil {
		t.Error("network failure")
	}
	for _, response := range []IconFetch{respond(200, pngBytes, http.Header{"Content-Length": {"99999999"}}),
		respond(200, make([]byte, maxIconBytes+1), http.Header{}), respond(200, []byte("<svg/>"), http.Header{})} {
		if LoadIcon("", cover, identity, response) != nil {
			t.Error("unsafe response accepted")
		}
	}
	_ = regexp.MustCompile
}
