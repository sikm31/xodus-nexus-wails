package steam

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var pngFixture = append([]byte("\x89PNG\r\n\x1a\n"), []byte("fixture")...)

var launch = Launch{Title: "A game", Executable: `"/bin/sh"`, Options: `"/home/me/launch-game" "0123456789abcdef"`, Directory: "/home/me",
	Identity: "profile:0123456789abcdef", Icon: "/home/me/icon.png"}

func notRunning() bool { return false }

func mustPlan(t *testing.T, original []byte, l Launch) Plan {
	t.Helper()
	plan, err := PlanShortcut(original, l)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func with(l Launch, change func(*Launch)) Launch { change(&l); return l }

func TestPlanShortcut(t *testing.T) {
	first := mustPlan(t, nil, launch)
	next := mustPlan(t, first.Bytes, with(launch, func(l *Launch) { l.Title, l.Options, l.Identity = "Second", "other", "other" }))
	if !bytes.Equal(next.Bytes[:len(first.Bytes)-2], first.Bytes[:len(first.Bytes)-2]) || next.AppID == first.AppID ||
		!bytes.Contains(first.Bytes, []byte("AllowOverlay")) || !bytes.Contains(first.Bytes, []byte("Xodus Nexus")) {
		t.Error("append")
	}
	renamed := bytes.Replace(first.Bytes, []byte("A game\x00"), []byte("Renamed by user\x00"), 1)
	if result := mustPlan(t, renamed, launch); result.Added || result.AppID != first.AppID || !bytes.Equal(result.Bytes, renamed) {
		t.Error("idempotent")
	}
	if second := mustPlan(t, first.Bytes, with(launch, func(l *Launch) { l.Options, l.Identity = "second", "second-profile" })); second.AppID == first.AppID {
		t.Error("identity separation")
	}
	if _, err := PlanShortcut(first.Bytes, with(launch, func(l *Launch) { l.Options = "changed" })); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Errorf("collision: %v", err)
	}
	for i, data := range [][]byte{{}, {0}, []byte("\x00other\x00\x08\x08"), []byte("\x00shortcuts\x00\x09bad\x00\x08\x08"), []byte("\x00shortcuts\x00\x08\x08junk")} {
		if _, err := PlanShortcut(data, launch); err == nil {
			t.Errorf("malformed %d accepted", i)
		}
	}
	if _, err := PlanShortcut(make([]byte, 32*1024*1024+1), launch); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized: %v", err)
	}
	if _, err := PlanShortcut(nil, with(launch, func(l *Launch) { l.Title = "bad\x00field" })); err == nil {
		t.Error("NUL accepted")
	}
	fields := bytes.Repeat([]byte("\x01x\x00\x00"), 500_001)
	compact := append(append([]byte("\x00shortcuts\x00\x000\x00"), fields...), 8, 8, 8)
	if _, err := PlanShortcut(compact, launch); err == nil || !strings.Contains(err.Error(), "Too many") {
		t.Errorf("field budget: %v", err)
	}
	if quoted, _ := Argument("/home/my \"$x`file`\\"); quoted != `"/home/my \"\$x\`+"`"+`file\`+"`"+`\\"` {
		t.Errorf("quoted = %s", quoted)
	}
	for _, bad := range []string{"bad\narg", "/home/%command%/test"} {
		if _, err := Argument(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func setup(t *testing.T, root string, data []byte) string {
	config := filepath.Join(root, ".local/share/Steam/userdata/123/config")
	os.MkdirAll(config, 0o700)
	for dir := config; dir != root; dir = filepath.Dir(dir) {
		os.Chmod(dir, 0o700)
	}
	if data != nil {
		os.WriteFile(filepath.Join(config, "shortcuts.vdf"), data, 0o600)
	}
	return config
}

func TestLargeImportedLibrary(t *testing.T) {
	root := t.TempDir()
	template := mustPlan(t, nil, with(launch, func(l *Launch) { l.Title, l.Options, l.Identity = "Imported game", "imported", "imported" })).Bytes
	header := len("\x00shortcuts\x00")
	entry := template[header : len(template)-2]
	unknown := append([]byte("\x01custom-provider\x00"), append(bytes.Repeat([]byte("x"), 600), 0)...)
	original := slices.Clone(template[:header])
	for i := range 12_000 {
		original = append(original, []byte("\x00"+strconv.Itoa(i)+"\x00")...)
		original = append(original, entry[3:len(entry)-1]...)
		original = append(original, unknown...)
		original = append(original, 8)
	}
	original = append(original, 8, 8)
	if len(original) <= 8*1024*1024 {
		t.Fatal("fixture too small")
	}
	config := setup(t, root, original)
	if _, err := AddShortcut(config, launch, Artwork{}, notRunning); err != nil {
		t.Fatal(err)
	}
	updated, _ := ReadShortcuts(filepath.Join(config, "shortcuts.vdf"))
	planned := mustPlan(t, updated, launch)
	found, _ := FindLaunches(updated, launch)
	if !bytes.Equal(updated[:len(original)-2], original[:len(original)-2]) || planned.Added || len(found) != 1 || found[0] != planned.AppID {
		t.Error("large library append")
	}
	entries, _ := os.ReadDir(config)
	index := slices.IndexFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), "shortcuts.vdf.nexus-backup-") })
	if backup, _ := os.ReadFile(filepath.Join(config, entries[index].Name())); !bytes.Equal(backup, original) {
		t.Error("backup differs")
	}
}

func TestReadAndAccounts(t *testing.T) {
	root := t.TempDir()
	config := setup(t, root, nil)
	file := filepath.Join(config, "shortcuts.vdf")
	handle, _ := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	handle.Truncate(32*1024*1024 + 1)
	handle.Close()
	if _, err := ReadShortcuts(file); err == nil || !strings.Contains(err.Error(), "Unsafe") {
		t.Errorf("oversized read: %v", err)
	}
	os.Remove(file)
	os.Mkdir(filepath.Join(root, ".steam"), 0o700)
	os.Symlink(filepath.Join(root, ".local/share/Steam"), filepath.Join(root, ".steam/steam"))
	flatpak := filepath.Join(root, ".var/app/com.valvesoftware.Steam/.local/share/Steam/userdata/456/config")
	os.MkdirAll(flatpak, 0o700)
	for dir := flatpak; dir != root; dir = filepath.Dir(dir) {
		os.Chmod(dir, 0o700)
	}
	if accounts, err := Accounts(root); err != nil || !slices.Equal(accounts, []string{config, flatpak}) {
		t.Errorf("accounts = %v, %v", accounts, err)
	}
}

func TestAddShortcut(t *testing.T) {
	root := t.TempDir()
	old := mustPlan(t, nil, with(launch, func(l *Launch) { l.Title, l.Options, l.Identity = "Unrelated", "unrelated", "unrelated" })).Bytes
	config := setup(t, root, old)
	uri, err := AddShortcut(config, launch, Artwork{Cover: pngFixture, Wide: pngFixture}, notRunning)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(config, "shortcuts.vdf"))
	result := mustPlan(t, data, launch)
	if uri != RunURI(result.AppID) || !bytes.Equal(result.Bytes[:len(old)-2], old[:len(old)-2]) {
		t.Errorf("uri = %s", uri)
	}
	if cover, _ := os.ReadFile(filepath.Join(config, "grid", strconv.FormatUint(uint64(result.AppID), 10)+"p.png")); !bytes.Equal(cover, pngFixture) {
		t.Error("artwork")
	}
	art := filepath.Join(config, "grid", strconv.FormatUint(uint64(result.AppID), 10)+"p.png")
	os.WriteFile(art, []byte("custom art"), 0o600)
	AddShortcut(config, launch, Artwork{Cover: pngFixture}, notRunning)
	if after, _ := os.ReadFile(filepath.Join(config, "shortcuts.vdf")); !bytes.Equal(after, data) {
		t.Error("duplicate entry")
	}
	if custom, _ := os.ReadFile(art); string(custom) != "custom art" {
		t.Error("custom art overwritten")
	}
}

func TestAddShortcutRefusals(t *testing.T) {
	root := t.TempDir()
	config := setup(t, root, nil)
	if _, err := AddShortcut(config, launch, Artwork{}, func() bool { return true }); err == nil || !strings.Contains(err.Error(), "Close Steam") {
		t.Errorf("running: %v", err)
	}
	if entries, _ := os.ReadDir(config); len(entries) != 0 {
		t.Error("files created while Steam runs")
	}
	var checks atomic.Int32
	if _, err := AddShortcut(config, launch, Artwork{}, func() bool { return checks.Add(1) >= 3 }); err == nil || !strings.Contains(err.Error(), "Close Steam") {
		t.Errorf("late start: %v", err)
	}
	entries, _ := os.ReadDir(config)
	for _, entry := range entries {
		if entry.Name() == "shortcuts.vdf" || strings.Contains(entry.Name(), ".lock") || strings.HasPrefix(entry.Name(), "shortcuts.vdf.nexus-") {
			t.Errorf("left %s", entry.Name())
		}
	}
	file := filepath.Join(config, "shortcuts.vdf")
	checks.Store(0)
	_, err := AddShortcut(config, launch, Artwork{}, func() bool {
		if checks.Add(1) == 2 {
			os.WriteFile(file, []byte("external edit"), 0o600)
		}
		return false
	})
	if data, _ := os.ReadFile(file); err == nil || !strings.Contains(err.Error(), "changed concurrently") || string(data) != "external edit" {
		t.Errorf("concurrent: %v", err)
	}
	for _, scenario := range []string{"file-symlink", "hardlink", "directory-symlink", "lock", "malformed"} {
		root := t.TempDir()
		config := setup(t, root, nil)
		file := filepath.Join(config, "shortcuts.vdf")
		target := filepath.Join(root, "untouched")
		os.WriteFile(target, []byte("untouched"), 0o600)
		selected := config
		switch scenario {
		case "file-symlink":
			os.Symlink(target, file)
		case "hardlink":
			os.Link(target, file)
		case "lock":
			os.WriteFile(filepath.Join(config, ".xodus-nexus-shortcuts.lock"), []byte("other operation"), 0o600)
		case "malformed":
			os.WriteFile(file, []byte("not VDF"), 0o600)
		case "directory-symlink":
			selected = filepath.Join(root, "alias")
			os.Symlink(config, selected)
		}
		if _, err := AddShortcut(selected, launch, Artwork{}, notRunning); err == nil {
			t.Errorf("%s accepted", scenario)
		}
		if data, _ := os.ReadFile(target); string(data) != "untouched" {
			t.Errorf("%s touched the target", scenario)
		}
	}
	plain := setup(t, t.TempDir(), nil)
	AddShortcut(plain, launch, Artwork{Cover: []byte("not PNG")}, notRunning)
	if entries, _ := os.ReadDir(filepath.Join(plain, "grid")); len(entries) != 0 {
		t.Error("malformed artwork published")
	}
}

var liveLaunch = Launch{Title: "Live title", Executable: `"/bin/sh"`, Options: `"/home/test/launch-game" "0123456789abcdef"`, Directory: "/home/test",
	Identity: "profile:game", Icon: "/home/test/icon.png"}

const assignedID = 0xf1234567

func assigned(t *testing.T, data []byte) []byte {
	if data == nil {
		data = mustPlan(t, nil, liveLaunch).Bytes
	}
	out := slices.Clone(data)
	index := bytes.Index(out, []byte("appid\x00")) + 6
	binary.LittleEndian.PutUint32(out[index:], assignedID)
	return out
}

var assignedURI = RunURI(assignedID)

func writeVDF(t *testing.T, config string, data []byte) {
	if err := os.WriteFile(filepath.Join(config, "shortcuts.vdf"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLiveConfirmation(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0o700)
	var requested string
	uri, err := AddLiveShortcut([]string{root}, liveLaunch, "/private/game.desktop", Artwork{}, func(file string) error {
		requested = file
		writeVDF(t, root, assigned(t, nil))
		return nil
	}, 15*time.Second, nil)
	if err != nil || uri != assignedURI || requested != "/private/game.desktop" {
		t.Fatalf("uri = %s, %v", uri, err)
	}
	for _, name := range names(root) {
		if strings.Contains(name, "backup") || strings.Contains(name, "lock") {
			t.Errorf("launcher modified Steam's files: %s", name)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(root, "shortcuts.vdf")); !bytes.Equal(data, assigned(t, nil)) {
		t.Error("Steam's VDF changed")
	}

	quoted := t.TempDir()
	actual := assigned(t, mustPlan(t, nil, with(liveLaunch, func(l *Launch) { l.Executable, l.Options = "/bin/sh", "/home/test/launch-game 0123456789abcdef" })).Bytes)
	if found, _ := FindLaunches(actual, liveLaunch); len(found) != 1 || found[0] != assignedID {
		t.Error("equivalent arguments")
	}
	writeVDF(t, quoted, actual)
	called := false
	uri, err = AddLiveShortcut([]string{quoted}, liveLaunch, "/private/game.desktop", Artwork{}, func(string) error { called = true; return nil }, 0,
		func(profiles []string) (string, error) { return profiles[0], nil })
	if err != nil || uri != assignedURI || called {
		t.Errorf("reuse = %s, %v, requested=%v", uri, err, called)
	}

	silent := t.TempDir()
	if _, err := AddLiveShortcut([]string{silent}, liveLaunch, "/private/game.desktop", Artwork{}, func(string) error { return nil }, 0, nil); err == nil || !strings.Contains(err.Error(), "has not confirmed") {
		t.Errorf("unconfirmed: %v", err)
	}
	if _, err := AddLiveShortcut([]string{silent}, liveLaunch, "/private/game.desktop", Artwork{}, func(string) error { return errors.New("request failed") }, 0, nil); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("request failure: %v", err)
	}
	if uri, err := AddLiveShortcut([]string{silent}, liveLaunch, "/private/game.desktop", Artwork{}, func(string) error { writeVDF(t, silent, assigned(t, nil)); return nil }, 0, nil); err != nil || uri != assignedURI {
		t.Errorf("guard not released: %v", err)
	}

	busy := t.TempDir()
	release := make(chan struct{})
	started := make(chan struct{})
	result := make(chan error)
	go func() {
		_, err := AddLiveShortcut([]string{busy}, liveLaunch, "/private/game.desktop", Artwork{}, func(string) error {
			close(started)
			<-release
			writeVDF(t, busy, assigned(t, nil))
			return nil
		}, time.Second, nil)
		result <- err
	}()
	<-started
	if _, err := AddLiveShortcut([]string{busy}, liveLaunch, "/other.desktop", Artwork{}, nil, 0, nil); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Errorf("overlap: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Error(err)
	}
}

func profiles(t *testing.T) []string {
	root := t.TempDir()
	var accounts []string
	for _, id := range []string{"123", "456"} {
		account := filepath.Join(root, "userdata", id, "config")
		os.MkdirAll(account, 0o700)
		accounts = append(accounts, account)
	}
	return accounts
}

func names(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

func TestProfileSelection(t *testing.T) {
	cover := []byte("\x89PNG\r\n\x1a\n")
	grid := strconv.FormatUint(assignedID, 10) + "p.png"
	accounts := profiles(t)
	uri, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{Cover: cover}, func(string) error {
		writeVDF(t, accounts[1], assigned(t, nil))
		return nil
	}, 0, nil)
	if data, _ := os.ReadFile(filepath.Join(accounts[1], "grid", grid)); err != nil || uri != assignedURI || !bytes.Equal(data, cover) || len(names(accounts[0])) != 0 {
		t.Errorf("session import: %v", err)
	}

	accounts = profiles(t)
	writeVDF(t, accounts[0], assigned(t, nil))
	var offered []string
	uri, err = AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{Cover: cover}, func(string) error {
		writeVDF(t, accounts[1], assigned(t, nil))
		return nil
	}, 0, func(existing []string) (string, error) { offered = existing; return "", nil })
	if err != nil || uri != assignedURI || !slices.Equal(offered, []string{accounts[0]}) || !slices.Equal(names(accounts[0]), []string{"shortcuts.vdf"}) {
		t.Errorf("inactive existing: %v %v", err, offered)
	}

	accounts = profiles(t)
	for _, account := range accounts {
		writeVDF(t, account, assigned(t, nil))
	}
	if _, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{}, nil, 0, nil); err == nil || !strings.Contains(err.Error(), "Choose") {
		t.Errorf("explicit reuse: %v", err)
	}
	if uri, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{Cover: cover}, func(string) error { t.Error("requested"); return nil }, 0,
		func(existing []string) (string, error) { return existing[1], nil }); err != nil || uri != assignedURI || !slices.Equal(names(accounts[0]), []string{"shortcuts.vdf"}) {
		t.Errorf("reuse second: %v", err)
	}
	if _, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{}, nil, 0, func([]string) (string, error) { return "", errors.New("cancelled") }); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("cancelled: %v", err)
	}
	if _, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{}, nil, 0, func([]string) (string, error) { return "/wrong", nil }); err == nil || !strings.Contains(err.Error(), "Invalid") {
		t.Errorf("invalid: %v", err)
	}

	accounts = profiles(t)
	both := func(string) error {
		for _, account := range accounts {
			writeVDF(t, account, assigned(t, nil))
		}
		return nil
	}
	if _, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{Cover: cover}, both, 0, nil); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("ambiguous: %v", err)
	}
	for _, account := range accounts {
		if !slices.Equal(names(account), []string{"shortcuts.vdf"}) {
			t.Error("artwork written while ambiguous")
		}
	}
	accounts = profiles(t)
	if uri, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{Cover: cover}, func(string) error {
		for _, account := range accounts {
			writeVDF(t, account, assigned(t, nil))
		}
		return nil
	}, 0, func(matching []string) (string, error) { return matching[1], nil }); err != nil || uri != assignedURI || !slices.Equal(names(accounts[0]), []string{"shortcuts.vdf"}) {
		t.Errorf("explicit choice: %v", err)
	}

	accounts = profiles(t)
	if uri, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{Cover: cover}, func(string) error {
		writeVDF(t, accounts[0], assigned(t, nil))
		writeVDF(t, accounts[1], []byte("partial"))
		go func() {
			time.Sleep(30 * time.Millisecond)
			writeVDF(t, accounts[1], mustPlan(t, nil, with(liveLaunch, func(l *Launch) { l.Options = "other" })).Bytes)
		}()
		return nil
	}, time.Second, nil); err != nil || uri != assignedURI {
		t.Errorf("partial write retry: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !slices.Equal(names(accounts[1]), []string{"shortcuts.vdf"}) {
		t.Error("artwork written to the partial profile")
	}

	accounts = profiles(t)
	writeVDF(t, accounts[1], []byte("malformed"))
	if _, err := AddLiveShortcut(accounts, liveLaunch, "/private/game.desktop", Artwork{}, func(string) error { t.Error("requested"); return nil }, 0, nil); err == nil {
		t.Error("unreadable snapshot accepted")
	}
}

func TestRequestImportRejectsLinks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "Game.desktop")
	os.WriteFile(file, []byte("[Desktop Entry]"), 0o600)
	alias := filepath.Join(root, "link.desktop")
	os.Symlink(file, alias)
	if err := RequestImport(alias); err == nil || !strings.Contains(err.Error(), "Unsafe") {
		t.Errorf("symlink: %v", err)
	}
	if err := RequestImport("relative.desktop"); err == nil {
		t.Error("relative path accepted")
	}
}

func TestArtwork(t *testing.T) {
	artwork, err := NexusArtwork()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"cover": artwork.Cover, "wide": artwork.Wide} {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := [2]int{600, 900}
		if name == "wide" {
			want = [2]int{1280, 720}
		}
		if got := [2]int{img.Bounds().Dx(), img.Bounds().Dy()}; got != want {
			t.Errorf("%s size = %v", name, got)
		}
	}
	portrait, err := GamePortrait(artwork.Wide)
	if err != nil {
		t.Fatal(err)
	}
	if img, _ := png.Decode(bytes.NewReader(portrait)); img.Bounds().Dx() != 600 || img.Bounds().Dy() != 900 {
		t.Error("portrait size")
	}
	if Normalize([]byte("not an image")) != nil || !isPNG(Normalize(artwork.Cover)) {
		t.Error("normalize")
	}
}
