package state

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"xodus-nexus-wails/internal/manifest"
	"xodus-nexus-wails/internal/shared"
)

func readable(file string) bool {
	handle, err := os.Open(file)
	if err != nil {
		return false
	}
	handle.Close()
	return true
}

func beginsWithMZ(file string) bool {
	handle, err := os.Open(file)
	if err != nil {
		return false
	}
	defer handle.Close()
	header := make([]byte, 2)
	n, _ := io.ReadFull(handle, header)
	return n == 2 && header[0] == 'M' && header[1] == 'Z'
}

func windowsPath(value string) string {
	return strings.ReplaceAll(value, `\`, string(filepath.Separator))
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// InspectGameDirectory reads an installed game's manifests and Xodus containers.
func InspectGameDirectory(directory string) (shared.Game, error) {
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return shared.Game{}, err
	}
	canonical, _ = filepath.Abs(canonical)
	// GDK titles are authored on Windows, where the manifest name is
	// case-insensitive; some ship "MicrosoftGame.Config", which a case-sensitive
	// Linux filesystem does not find by the canonical name.
	entries, _ := os.ReadDir(canonical)
	find := func(name, fallback string) string {
		for _, entry := range entries {
			if strings.ToLower(entry.Name()) == name {
				return filepath.Join(canonical, entry.Name())
			}
		}
		return filepath.Join(canonical, fallback)
	}
	configSource, _ := os.ReadFile(find("microsoftgame.config", "MicrosoftGame.config"))
	appxSource, _ := os.ReadFile(find("appxmanifest.xml", "appxmanifest.xml"))
	if len(configSource) == 0 && len(appxSource) == 0 {
		return shared.Game{}, errors.New("The selected directory does not contain MicrosoftGame.config or appxmanifest.xml.")
	}
	var config, appx *manifest.Node
	if len(configSource) > 0 {
		if config, err = manifest.Parse(string(configSource)); err != nil {
			return shared.Game{}, err
		}
	}
	if len(appxSource) > 0 {
		if appx, err = manifest.Parse(string(appxSource)); err != nil {
			return shared.Game{}, err
		}
	}
	title := first(manifest.Attr(config, "ShellVisuals", "DefaultDisplayName"), manifest.Text(appx, "DisplayName"), filepath.Base(canonical))
	executable := first(manifest.Attr(config, "Executable", "Name"), manifest.Attr(appx, "Application", "Executable"))
	hasFinal := readable(filepath.Join(canonical, ".xodus-streaming.msixvc"))
	hasTemporary := readable(filepath.Join(canonical, ".xodus-streaming-tmp.msixvc"))
	exeExists := false
	if executable != "" {
		_, exeExists = manifest.ResolvePackageFile(canonical, executable)
	}
	vcRedist := filepath.Join(canonical, "Engine", "Extras", "Redist", "en-us", "vc_redist.x64.exe")
	needsVC := slices.ContainsFunc(manifest.Elements(config, "KnownDependency"), func(node *manifest.Node) bool {
		name, _ := manifest.AttributeValue(node, "Name")
		return strings.ToUpper(name) == "VC14"
	})
	vcRedistEncrypted := needsVC && readable(vcRedist) && !beginsWithMZ(vcRedist)

	var antiCheat *shared.AntiCheat
	for _, directory := range []string{"EasyAntiCheat", "EasyAntiCheat_EOS"} {
		data, err := os.ReadFile(filepath.Join(canonical, directory, "Settings.json"))
		if err != nil {
			continue
		}
		var settings map[string]any
		if json.Unmarshal(data, &settings) != nil {
			continue
		}
		target, _ := settings["executable"].(string)
		if strings.TrimSpace(target) == "" {
			target = ""
		}
		encrypted := target != "" && !beginsWithMZ(filepath.Join(canonical, windowsPath(target)))
		antiCheat = &shared.AntiCheat{Provider: "Easy Anti-Cheat", Executable: target, EncryptedTarget: encrypted}
		break
	}

	coverDataURL := ""
	if cover := first(manifest.Attr(config, "ShellVisuals", "Square480x480Logo"), manifest.Attr(config, "ShellVisuals", "Square150x150Logo"),
		manifest.Attr(appx, "VisualElements", "Square150x150Logo"), manifest.Attr(appx, "VisualElements", "Square44x44Logo")); cover != "" {
		// Artwork is optional.
		if data, err := os.ReadFile(filepath.Join(canonical, windowsPath(cover))); err == nil && len(data) < 3_000_000 {
			coverDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
		}
	}

	installState := "imported"
	if hasFinal && exeExists {
		installState = "ready"
	} else if hasTemporary {
		installState = "incomplete"
	}
	installMessage := ""
	switch {
	case installState == "incomplete":
		installMessage = "A temporary Xodus container was found; the download was not completed."
	case installState == "imported" && !hasFinal:
		installMessage = "The .xodus-streaming.msixvc container is missing; Xodus run may not work."
	case installState == "imported" && !exeExists:
		installMessage = "The manifest executable is missing, ambiguous, or outside the installation directory."
	}
	digest := sha256.Sum256([]byte(canonical))
	return shared.Game{
		ID: hex.EncodeToString(digest[:])[:16], Title: title, Path: canonical,
		StoreID:          manifest.Text(config, "StoreId"),
		Publisher:        first(manifest.Attr(config, "ShellVisuals", "PublisherDisplayName"), manifest.Text(appx, "PublisherDisplayName")),
		InstalledVersion: first(manifest.Attr(config, "Identity", "Version"), manifest.Attr(appx, "Identity", "Version")),
		PackageName:      first(manifest.Attr(config, "Identity", "Name"), manifest.Attr(appx, "Identity", "Name")),
		Executable:       executable, CoverDataURL: coverDataURL, InstallState: installState, InstallMessage: installMessage,
		VCRedistEncrypted: &vcRedistEncrypted, AntiCheat: antiCheat, AddedAt: shared.JSISOString(time.Now()),
	}, nil
}
