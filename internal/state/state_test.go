package state

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"xodus-nexus-wails/internal/shared"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func inspect(t *testing.T, directory string) shared.Game {
	t.Helper()
	game, err := InspectGameDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	return game
}

func TestBandwidthSettings(t *testing.T) {
	directory := t.TempDir()
	store := New(directory)
	store.Load()
	if *store.Settings().DownloadLimitMiB != 0 {
		t.Error("default is unlimited")
	}
	parallel := store.Settings().ParallelDownloads
	if _, err := store.SetSettings(Patch{"downloadLimitMiB": 0.5}); err != nil {
		t.Fatal(err)
	}
	reopened := New(directory)
	reopened.Load()
	if *reopened.Settings().DownloadLimitMiB != 0.5 || reopened.Settings().ParallelDownloads != parallel {
		t.Errorf("reopened = %+v", reopened.Settings())
	}
	if _, err := reopened.SetSettings(Patch{"downloadLimitMiB": -1.0}); err == nil || !strings.Contains(err.Error(), "Download speed limit") {
		t.Errorf("negative: %v", err)
	}
	if *reopened.Settings().DownloadLimitMiB != 0.5 {
		t.Error("invalid patch must not apply")
	}
	for _, value := range []string{"null", "-1", `"2"`, "2048", "0.000001"} {
		write(t, filepath.Join(directory, "launcher-state.json"), `{"settings":{"downloadLimitMiB":`+value+`}}`)
		store := New(directory)
		store.Load()
		if *store.Settings().DownloadLimitMiB != 0 {
			t.Errorf("%s loaded as %v", value, *store.Settings().DownloadLimitMiB)
		}
	}
}

func TestManifestCommentsIgnored(t *testing.T) {
	directory := t.TempDir()
	xml := "<?xml version=\"1.0\"?>\n<!-- examples -> \x1b[32m <Identity Name=\"Example\" Version=\"0.0.0.0\"/>\n" +
		"<ShellVisuals DefaultDisplayName=\"Example\" Square480x480Logo=\"wrong.png\"/>\n<Executable Name=\"Wrong.exe\"/><StoreId>000000000000</StoreId>\n" +
		"<KnownDependency Name=\"VC14\"/> -->\n<Game><Identity Name='Actual.Package' Version='1.2.3.4'/>\n" +
		"<ShellVisuals DefaultDisplayName=\"Actual &amp; Game\" PublisherDisplayName=\"Publisher\"\n Square480x480Logo=\"actual.png\"/>\n" +
		"<ExecutableList><Executable Name='Game.exe'/></ExecutableList>\n<StoreId>9AAAAA<!-- annotation -->AAAAAA</StoreId></Game>"
	write(t, filepath.Join(directory, "MicrosoftGame.config"), xml)
	write(t, filepath.Join(directory, "Game.exe"), "MZ fixture")
	write(t, filepath.Join(directory, ".xodus-streaming.msixvc"), "fixture")
	write(t, filepath.Join(directory, "actual.png"), "actual image")
	write(t, filepath.Join(directory, "Engine/Extras/Redist/en-us/vc_redist.x64.exe"), "encrypted fixture")
	game := inspect(t, directory)
	if game.Title != "Actual & Game" || game.StoreID != "9AAAAAAAAAAA" || game.PackageName != "Actual.Package" || game.InstalledVersion != "1.2.3.4" ||
		game.Executable != "Game.exe" || game.Publisher != "Publisher" || *game.VCRedistEncrypted || game.InstallState != "ready" ||
		game.CoverDataURL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("actual image")) {
		t.Errorf("game = %+v", game)
	}
	if data, _ := os.ReadFile(filepath.Join(directory, "MicrosoftGame.config")); string(data) != xml {
		t.Error("manifest changed")
	}

	absent := t.TempDir()
	write(t, filepath.Join(absent, "MicrosoftGame.config"), `<Game><!-- <StoreId>000000000000</StoreId><Executable Name="Wrong.exe"/> --><Description><![CDATA[<StoreId>111111111111</StoreId>]]></Description></Game>`)
	if game := inspect(t, absent); game.StoreID != "" || game.Executable != "" || game.InstallState != "imported" {
		t.Errorf("absent = %+v", game)
	}

	appx := t.TempDir()
	write(t, filepath.Join(appx, "appxmanifest.xml"), `<Package xmlns:uap="urn:test">
      <!-- <Identity Name="Wrong"/><DisplayName>Wrong</DisplayName><Application Executable="Wrong.exe"/> -->
      <Identity Name="Actual.Appx" Version="2.0.0.0"/>
      <Properties><DisplayName>Actual &gt; Example</DisplayName><PublisherDisplayName>Actual</PublisherDisplayName></Properties>
      <Applications><Application Executable='Actual.exe'><uap:VisualElements Description="a > b" Square150x150Logo='actual.png'/></Application></Applications></Package>`)
	write(t, filepath.Join(appx, "actual.png"), "actual")
	if game := inspect(t, appx); game.Title != "Actual > Example" || game.PackageName != "Actual.Appx" || game.Executable != "Actual.exe" ||
		game.Publisher != "Actual" || game.InstalledVersion != "2.0.0.0" || game.CoverDataURL != "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("actual")) {
		t.Errorf("appx = %+v", game)
	}

	malformed := t.TempDir()
	write(t, filepath.Join(malformed, "MicrosoftGame.config"), "<Game><!-- <StoreId>000000000000</StoreId></Game>")
	if _, err := InspectGameDirectory(malformed); err == nil {
		t.Error("unterminated comment accepted")
	}
}

func TestMixedCaseExecutable(t *testing.T) {
	root := t.TempDir()
	manifestPath := `bin\Gaming.Desktop.x64\witcher3.exe`
	write(t, filepath.Join(root, "MicrosoftGame.Config"), `<Game><ExecutableList><Executable Name="`+manifestPath+`" /></ExecutableList></Game>`)
	write(t, filepath.Join(root, ".xodus-streaming.msixvc"), "inspection fixture")
	write(t, filepath.Join(root, "bin/gaming.desktop.x64/WITCHER3.EXE"), "encrypted fixture")
	if game := inspect(t, root); game.InstallState != "ready" || game.Executable != manifestPath {
		t.Errorf("game = %+v", game)
	}
	os.Remove(filepath.Join(root, ".xodus-streaming.msixvc"))
	if inspect(t, root).InstallState != "imported" {
		t.Error("missing container")
	}
}

func eacGame(t *testing.T, header []byte, directory string) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "MicrosoftGame.config"), `<Game><ExecutableList><Executable Name="start_protected_game.exe" /></ExecutableList><StoreId>TEST123</StoreId></Game>`)
	write(t, filepath.Join(root, ".xodus-streaming.msixvc"), "container")
	write(t, filepath.Join(root, "start_protected_game.exe"), "encrypted launcher")
	write(t, filepath.Join(root, directory, "Settings.json"), `{"executable":"Game\\Binaries\\Game.exe"}`)
	write(t, filepath.Join(root, "Game", "Binaries", "Game.exe"), string(header))
	return root
}

func TestAntiCheatAndPrerequisites(t *testing.T) {
	game := inspect(t, eacGame(t, []byte{1, 2, 3}, "EasyAntiCheat"))
	if game.InstallState != "ready" || game.Executable != "start_protected_game.exe" ||
		*game.AntiCheat != (shared.AntiCheat{Provider: "Easy Anti-Cheat", Executable: `Game\Binaries\Game.exe`, EncryptedTarget: true}) {
		t.Errorf("encrypted EAC = %+v %+v", game, game.AntiCheat)
	}
	if game := inspect(t, eacGame(t, []byte("MZ executable"), "EasyAntiCheat")); game.AntiCheat.EncryptedTarget {
		t.Error("PE target is not encrypted")
	}
	if game := inspect(t, eacGame(t, []byte("MZ executable"), "EasyAntiCheat_EOS")); game.AntiCheat == nil || game.AntiCheat.Provider != "Easy Anti-Cheat" {
		t.Error("EOS layout")
	}
	plain := t.TempDir()
	write(t, filepath.Join(plain, "MicrosoftGame.config"), `<Game><ExecutableList><Executable Name="Game.exe" /></ExecutableList><KnownDependency Name="VC14" /></Game>`)
	write(t, filepath.Join(plain, ".xodus-streaming.msixvc"), "container")
	write(t, filepath.Join(plain, "Game.exe"), "MZ executable")
	if game := inspect(t, plain); game.AntiCheat != nil || *game.VCRedistEncrypted {
		t.Errorf("plain = %+v", game)
	}
	write(t, filepath.Join(plain, "Engine", "Extras", "Redist", "en-us", "vc_redist.x64.exe"), "\x00\x01\x02")
	if !*inspect(t, plain).VCRedistEncrypted {
		t.Error("encrypted VC redistributable")
	}
}

func TestLaunchToolsSurviveRefresh(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	game := shared.Game{ID: "test-tools", Title: "Synthetic game", Path: "/sample/game", InstallState: "ready", AddedAt: "2026-10-16T00:00:00Z"}
	on := true
	tools := shared.GameLaunchTools{DualSenseCompatibility: &on, MangoHud: true, LaunchOptions: "DXVK_HUD=fps %command% --launcher-skip"}
	store.UpsertGame(game)
	store.PatchGame(game.ID, Patch{"launchTools": ToPatch(tools)})
	reopened := New(root)
	reopened.Load()
	if got := reopened.Games()[0].LaunchTools; got == nil || got.LaunchOptions != tools.LaunchOptions || !*got.DualSenseCompatibility || !got.MangoHud {
		t.Fatalf("reopened tools = %+v", got)
	}
	game.Publisher = "Updated metadata"
	reopened.UpsertGame(game)
	if got := reopened.Games()[0]; got.LaunchTools == nil || got.LaunchTools.LaunchOptions != tools.LaunchOptions || got.Publisher != "Updated metadata" {
		t.Errorf("after refresh = %+v", got)
	}
}

func TestLegacyProfileAndDefaults(t *testing.T) {
	directory := t.TempDir()
	write(t, filepath.Join(directory, "launcher-state.json"), `{"games":[{"id":"game","title":"Any title","path":"/package","executable":"Launcher.exe","launchSupport":"offline-only-eac","launchBlockReason":"Old warning","installState":"ready"}]}`)
	store := New(directory)
	store.Load()
	if store.Games()[0].Executable != "Launcher.exe" {
		t.Error("legacy game lost")
	}
	store.Save()
	if data, _ := os.ReadFile(filepath.Join(directory, "launcher-state.json")); strings.Contains(string(data), "launchSupport") {
		t.Error("legacy route fields kept")
	}
	settings := New(t.TempDir()).Settings()
	if !strings.Contains(settings.InstallRoot, filepath.Join("Games", "Xodus Nexus Wails")) || settings.ExternalXodusRoot != "" || settings.VerboseLogs {
		t.Errorf("defaults = %+v", settings)
	}
}

func TestQueueHistory(t *testing.T) {
	directory := t.TempDir()
	store := New(directory)
	base := shared.DownloadTask{Title: "Sample", ProductID: "9AAAAAAAAAAA", Destination: "/sample", Status: "queued", Phase: "Waiting", Log: []string{}}
	var group sync.WaitGroup
	for i := range 40 {
		group.Add(1)
		go func() {
			defer group.Done()
			task := base
			task.ID = fmt.Sprintf("pending-%d", i)
			store.AddDownload(task)
		}()
	}
	group.Wait()
	for i := range 35 {
		group.Add(1)
		go func() {
			defer group.Done()
			task := base
			task.ID, task.Status = fmt.Sprintf("done-%d", i), "complete"
			store.AddDownload(task)
		}()
	}
	group.Wait()
	count := func(tasks []shared.DownloadTask, status string) int {
		n := 0
		for _, task := range tasks {
			if task.Status == status {
				n++
			}
		}
		return n
	}
	if count(store.Downloads(), "queued") != 40 || count(store.Downloads(), "complete") != 30 {
		t.Errorf("queued %d complete %d", count(store.Downloads(), "queued"), count(store.Downloads(), "complete"))
	}
	loaded := New(directory)
	loaded.Load()
	if len(loaded.Downloads()) != 70 || count(loaded.Downloads(), "paused") != 40 {
		t.Errorf("loaded %d paused %d", len(loaded.Downloads()), count(loaded.Downloads(), "paused"))
	}
	if task, _ := loaded.Download("pending-0"); !task.CanPause {
		t.Error("restored task must be resumable")
	}
}

func TestRemoveOnlyFinished(t *testing.T) {
	directory := t.TempDir()
	store := New(directory)
	destination := filepath.Join(directory, "game")
	write(t, filepath.Join(destination, "keep.txt"), "game data")
	for _, status := range []string{"running", "paused", "failed"} {
		store.AddDownload(shared.DownloadTask{ID: status, Title: "Game", ProductID: "TEST", Destination: destination, Status: status, Log: []string{}})
	}
	for _, id := range []string{"running", "paused"} {
		if err := store.RemoveDownload(id); err == nil || !strings.Contains(err.Error(), "Stop the installation") {
			t.Errorf("%s: %v", id, err)
		}
	}
	store.RemoveDownload("failed")
	ids := func(tasks []shared.DownloadTask) []string {
		var out []string
		for _, task := range tasks {
			out = append(out, task.ID)
		}
		return out
	}
	if !slices.Equal(ids(store.Downloads()), []string{"paused", "running"}) {
		t.Errorf("ids = %v", ids(store.Downloads()))
	}
	store.ClearFinishedDownloads()
	restored := New(directory)
	restored.Load()
	for _, task := range restored.Downloads() {
		if task.Status != "paused" || !task.CanPause || task.ControlPending {
			t.Errorf("restored %+v", task)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(destination, "keep.txt")); string(data) != "game data" {
		t.Error("install files touched")
	}
}

func TestArtworkAndQuotedTitles(t *testing.T) {
	caseVariant := t.TempDir()
	write(t, filepath.Join(caseVariant, "MicrosoftGame.Config"), `<Game><ShellVisuals DefaultDisplayName="Forza Horizon 6" Square150x150Logo="Logo.png" /><ExecutableList><Executable Name="Game.exe" /></ExecutableList></Game>`)
	write(t, filepath.Join(caseVariant, "Logo.png"), "PNGDATA")
	if game := inspect(t, caseVariant); game.Title != "Forza Horizon 6" || game.CoverDataURL != "data:image/png;base64,UE5HREFUQQ==" {
		t.Errorf("case variant = %+v", game)
	}
	appx := t.TempDir()
	write(t, filepath.Join(appx, "MicrosoftGame.config"), `<Game><ExecutableList><Executable Name="Game.exe" /></ExecutableList></Game>`)
	write(t, filepath.Join(appx, "appxmanifest.xml"), `<Package><Applications><Application Id="App" Executable="Game.exe"><uap:VisualElements DisplayName="Forza Horizon 6" Square44x44Logo="Small.png" Square150x150Logo="Logo.png" /></Application></Applications></Package>`)
	write(t, filepath.Join(appx, "Logo.png"), "PNGDATA")
	if inspect(t, appx).CoverDataURL != "data:image/png;base64,UE5HREFUQQ==" {
		t.Error("appx cover")
	}
	quoted := t.TempDir()
	write(t, filepath.Join(quoted, "MicrosoftGame.config"), `<Game><ExecutableList><Executable Name="Game.exe" /></ExecutableList><ShellVisuals DefaultDisplayName="No Man's Sky" PublisherDisplayName="Hello Games" /></Game>`)
	if game := inspect(t, quoted); game.Title != "No Man's Sky" || game.Publisher != "Hello Games" {
		t.Errorf("apostrophe = %+v", game)
	}
	write(t, filepath.Join(quoted, "MicrosoftGame.config"), `<Game><ShellVisuals DefaultDisplayName='A "quoted" Game' /></Game>`)
	if inspect(t, quoted).Title != `A "quoted" Game` {
		t.Error("double quote in single-quoted attribute")
	}
}
