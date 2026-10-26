package shared

import "time"

// Demo fixtures are served when the launcher runs with XODUS_DEMO=1, so
// screenshots and UI tours show plausible content without any account,
// backend or installed games. Store pages keep using the live Microsoft Store
// catalog, and library covers are fetched from the Store by the real Product
// IDs below. No real credentials, tokens or account data.

type DemoArtwork struct{ CoverURL, HeroURL string }

type DemoVersions struct {
	Launcher, Backend, BackendCommit, BackendCompatibility string
}

func ptrFloat(v float64) *float64 { return &v }

// DemoGames use real Microsoft Store product IDs so display-catalog lookups
// return the genuine published artwork.
func DemoGames() []Game {
	return []Game{
		{ID: "demo-1", Title: "Forza Horizon 6", Path: "/home/demo/Games/Xodus Nexus/ForzaHorizon6", StoreID: "9N431PX143P8", Publisher: "Xbox Game Studios", Executable: "ForzaHorizon6.exe", InstallState: "ready", AddedAt: "2026-10-08T18:12:00.000Z"},
		{ID: "demo-2", Title: "Gears of War: E-Day", Path: "/home/demo/Games/Xodus Nexus/GearsEDay", StoreID: "9N4PT8HGCDHQ", Publisher: "The Coalition", Executable: "start_protected_game.exe", InstallState: "ready", AntiCheat: &AntiCheat{Provider: "Easy Anti-Cheat", EncryptedTarget: true}, AddedAt: "2026-10-08T21:40:00.000Z"},
		{ID: "demo-3", Title: "Minecraft Dungeons II", Path: "/home/demo/Games/Xodus Nexus/Minecraft-Dungeons-II", StoreID: "9PM3J18QM4Q3", Publisher: "Mojang", Executable: "Binaries/WinGDK/Dungeons.exe", InstallState: "ready", AddedAt: "2026-10-09T09:05:00.000Z"},
		{ID: "demo-4", Title: "Sea of Thieves", Path: "/home/demo/Games/Xodus Nexus/SeaOfThieves", StoreID: "9P2N57MC619K", Publisher: "Rare", Executable: "SoTGame.exe", InstallState: "incomplete", InstallMessage: "Package verification pending", AddedAt: "2026-10-10T14:22:00.000Z"},
		{ID: "demo-5", Title: "Grounded", Path: "/home/demo/Games/Xodus Nexus/Grounded", StoreID: "9PJTHRNVH62H", Publisher: "Obsidian", Executable: "Indiana/Binaries/WinGDK/Indiana.exe", InstallState: "imported", AddedAt: "2026-10-10T16:48:00.000Z"},
		{ID: "demo-6", Title: "A Game About Digging A Hole", Path: "/home/demo/Games/Xodus Nexus/DiggingHole", StoreID: "9NGLST31DG26", Publisher: "Local import", Executable: "Dig.exe", InstallState: "ready", AddedAt: "2026-10-11T10:02:00.000Z"},
	}
}

func DemoDownloads() []DownloadTask {
	now := JSISOString(time.Now())
	return []DownloadTask{
		{ID: "demo-download-1", Kind: "install", Title: "Age of Mythology: Retold", ProductID: "9NPKGVXPFRSX", Destination: "/home/demo/Games/Xodus Nexus", Status: "running",
			Progress: ptrFloat(42), DownloadedBytes: ptrFloat(18_612_245_504), TotalBytes: ptrFloat(44_258_432_000), TransferRate: "86.4 MiB/s", Phase: "Streaming package", Log: []string{}, StartedAt: now},
		{ID: "demo-download-2", Kind: "install", Title: "Sea of Thieves", ProductID: "9P2N57MC619K", Destination: "/home/demo/Games/Xodus Nexus", Status: "queued", Phase: "Queued", Log: []string{}, StartedAt: now},
		{ID: "demo-download-3", Kind: "install", Title: "Grounded", ProductID: "9PJTHRNVH62H", Destination: "/home/demo/Games/Xodus Nexus", Status: "complete", Progress: ptrFloat(100), Phase: "Complete", Log: []string{}, StartedAt: now, FinishedAt: now},
	}
}

// DemoAccountProducts are explicit sample entitlements, never inferred from
// or fetched for a real user.
var DemoAccountProducts = []struct{ ProductID, Title, Publisher, Access string }{
	{"9N26X2PMTWBT", "Wolfenstein II: The New Colossus", "Bethesda Softworks", "owned"},
	{"9P74BRFBXXC6", "DOOM Eternal (BATTLEMODE - PC)", "Bethesda Softworks", "owned"},
	{"9NPKGVXPFRSX", "Age of Mythology: Retold", "Xbox Game Studios", "gamepass"},
}

func DemoAccountLibrary(kind string, artwork map[string]DemoArtwork) AccountLibraryPage {
	access := map[string]LibraryAccess{}
	for index, game := range DemoGames() {
		if game.StoreID != "" {
			access[game.StoreID] = "gamepass"
			if index == 1 {
				access[game.StoreID] = "owned"
			}
		}
	}
	products := []LibraryProduct{}
	for _, product := range DemoAccountProducts {
		access[product.ProductID] = product.Access
		if kind != "all" && product.Access != kind {
			continue
		}
		products = append(products, LibraryProduct{Access: product.Access, CatalogProduct: CatalogProduct{ProductID: product.ProductID, Title: product.Title,
			Publisher: product.Publisher, Description: "Sample library entry for the account-free demo.", Categories: []string{},
			PosterURL: artwork[product.ProductID].CoverURL, HeroURL: artwork[product.ProductID].HeroURL, Installability: "supported", SupportNote: "Demo data only."}})
	}
	return AccountLibraryPage{Kind: kind, CheckedAt: JSISOString(time.Now()), AccessByProductID: access, Products: products}
}

func DemoSnapshot(versions DemoVersions, artwork map[string]DemoArtwork) AppSnapshot {
	if versions == (DemoVersions{}) {
		versions = DemoVersions{"0.0.0", "0.0.0", "0000000000000000000000000000000000000000", "demo"}
	}
	stored := true
	games := DemoGames()
	ordered := append([]Game{games[1], games[0]}, games[2:]...)
	for i := range ordered {
		if art, ok := artwork[ordered[i].StoreID]; ok {
			ordered[i].CoverURL, ordered[i].HeroURL = art.CoverURL, art.HeroURL
		}
	}
	downloads := DemoDownloads()
	for i := range downloads {
		downloads[i].CoverURL = artwork[downloads[i].ProductID].CoverURL
	}
	return AppSnapshot{
		Backend: BackendStatus{Available: true, Source: "managed", BinaryPath: "/home/demo/.config/xodus-nexus/xodus-backends/current", Commit: versions.BackendCommit,
			UpstreamCommit: versions.BackendCommit, Capabilities: BackendCapabilities{Commands: []string{"login", "logout", "download", "streaming", "run", "license", "extract", "profile", "library"},
				Account: true, Profile: true, Library: true, Login: true, ExternalAuth: true, Logout: true, Download: true, Streaming: true, StreamingProgressJSON: true,
				StreamingControls: true, Run: true, Version: "0.5.0"},
			Prerequisites: BackendPrerequisites{true, true, true, true}, LinuxRunPatch: versions.BackendCompatibility, Version: versions.Backend,
			Delivery: "release", ReleaseAvailable: true, ReleaseVersion: versions.Backend},
		Runtime: RuntimeStatus{Installed: true, RuntimeRoot: "/home/demo/.local/share/xodus-nexus/runtimes", Name: "xodus-nexus-runtime-demo",
			WinePath: "/home/demo/.local/share/xodus-nexus/runtimes/demo/files/bin/wine", Prerequisites: RuntimePrerequisites{true, true, true, true}},
		Games:     ordered,
		Downloads: downloads,
		Settings:  Settings{InstallRoot: "/home/demo/Games/Xodus Nexus", Market: "US", ParallelDownloads: 4, FirstRunCompleted: true},
		Auth: AuthStatus{Stored: &stored, Username: "sample@example.test", Gamertag: "NexusPlayer",
			Subscription: &GamePassSubscription{Status: "active", ProductIDs: []string{"CFQ7TTC0KGQ8"}, CheckedAt: JSISOString(time.Now())},
			LastResult:   "success", Note: "Demo mode: sample data only. Store pages show the live Microsoft Store catalog."},
		LauncherUpdate: &LauncherUpdate{CurrentVersion: versions.Launcher, CheckedAt: JSISOString(time.Now())},
	}
}

var (
	DemoLaunchOptions = GameLaunchOptions{CloudSaves: true, VCInstall: true}
	DemoLocales       = []string{"en-US", "en-GB", "cs-CZ", "de-DE", "fr-FR", "ja-JP"}
)
