package manifest

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

const manifestLimit = 1_048_576

var hexTitle = regexp.MustCompile(`^[0-9A-Fa-f]+$`)

func readManifest(directory, filename string) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	var names []string
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), filename) {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	if len(names) != 1 {
		return "", errors.New("Ambiguous game language manifest.")
	}
	handle, err := os.OpenFile(filepath.Join(directory, names[0]), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	stat, err := handle.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() > manifestLimit {
		return "", errors.New("Unsupported game language manifest.")
	}
	data, err := io.ReadAll(io.LimitReader(handle, manifestLimit+1))
	if err != nil {
		return "", err
	}
	if len(data) > manifestLimit {
		return "", errors.New("Game language manifest is too large.")
	}
	return strings.ToValidUTF8(string(data), "�"), nil
}

func parseOptional(xml string) (*Node, error) {
	if xml == "" {
		return nil, nil
	}
	return Parse(xml)
}

func InstalledGameLocales(directory string) ([]string, error) {
	var found []string
	seen := map[string]bool{}
	for _, file := range []string{"MicrosoftGame.config", "AppxManifest.xml"} {
		xml, err := readManifest(directory, file)
		if err != nil {
			return nil, err
		}
		locales, err := Locales(xml)
		if err != nil {
			return nil, err
		}
		for _, locale := range locales {
			if !seen[locale] {
				seen[locale] = true
				found = append(found, locale)
			}
		}
	}
	if found == nil {
		found = []string{}
	}
	return found, nil
}

func InstalledGameTitleID(directory string) (string, error) {
	xml, err := readManifest(directory, "MicrosoftGame.config")
	if err != nil {
		return "", err
	}
	return TitleID(xml)
}

// Locales returns the normalized resource languages declared by a manifest.
func Locales(xml string) ([]string, error) {
	root, err := parseOptional(xml)
	if err != nil {
		return nil, err
	}
	found := []string{}
	seen := map[string]bool{}
	for _, tag := range Elements(root, "Resource") {
		value, _ := AttributeValue(tag, "Language")
		if value == "" {
			continue
		}
		// Resource placeholders are not locale overrides.
		if locale, err := shared.NormalizeGameLocale(value); err == nil && !seen[locale] {
			seen[locale] = true
			found = append(found, locale)
		}
	}
	return found, nil
}

func TitleID(xml string) (string, error) {
	root, err := parseOptional(xml)
	if err != nil {
		return "", err
	}
	value := Text(root, "TitleId")
	if value != "" && hexTitle.MatchString(value) {
		return value, nil
	}
	return "", nil
}

// SupportedGameLocale returns "" for no override. Resources localize shell
// visuals, not necessarily every game translation.
func SupportedGameLocale(requested string, locales []string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return "", nil
	}
	normalized, err := shared.NormalizeGameLocale(requested)
	if err != nil {
		return "", err
	}
	if _, err := shared.WindowsGameLocale(normalized); err != nil {
		return "", err
	}
	for _, locale := range locales {
		if strings.EqualFold(locale, normalized) {
			return locale, nil
		}
	}
	return normalized, nil
}

var localeVariables = []string{"LC_ALL", "LC_MESSAGES", "LC_CTYPE", "LC_TIME", "LC_COLLATE", "LC_MONETARY", "LC_NUMERIC", "LC_PAPER"}

// GameLocaleEnvironment sets Windows and Unix locale only for the selected game.
func GameLocaleEnvironment(source launcherenv.Env, locale string) (launcherenv.Env, error) {
	env := source.Clone()
	delete(env, "XODUS_PACKAGE_LOCALE")
	delete(env, "WINEUSERLOCALE")
	if locale == "" {
		return env, nil
	}
	tag, err := shared.NormalizeGameLocale(locale)
	if err != nil {
		return nil, err
	}
	windows, err := shared.WindowsGameLocale(tag)
	if err != nil {
		return nil, err
	}
	resolved, err := shared.ParseLocale(windows)
	if err != nil {
		return nil, err
	}
	modifier := ""
	switch resolved.Script {
	case "":
	case "Latn":
		modifier = "@latin"
	case "Cyrl":
		modifier = "@cyrillic"
	default:
		modifier = "@" + strings.ToLower(resolved.Script)
	}
	unix := resolved.Language + "_" + resolved.Region + ".UTF-8" + modifier
	env["XODUS_PACKAGE_LOCALE"] = tag
	env["WINEUSERLOCALE"] = windows
	env["LANG"] = unix
	env["LANGUAGE"] = resolved.Language + "_" + resolved.Region + modifier + ":" + resolved.Language
	for _, key := range localeVariables {
		env[key] = unix
	}
	return env, nil
}

var localeSplit = regexp.MustCompile(`[.@]`)

// SystemGameLocale maps the desktop locale to a Windows locale, or "".
func SystemGameLocale(env launcherenv.Env) string {
	value := env["LC_ALL"]
	if value == "" {
		value = env["LC_MESSAGES"]
	}
	if value == "" {
		value = env["LANG"]
	}
	if value == "C" || value == "POSIX" || strings.HasPrefix(value, "C.") {
		return "en-US"
	}
	tag := ""
	if value != "" {
		base := strings.ReplaceAll(localeSplit.Split(value, 2)[0], "_", "-")
		script := ""
		if strings.HasSuffix(value, "@latin") {
			script = "Latn"
		} else if strings.HasSuffix(value, "@cyrillic") {
			script = "Cyrl"
		}
		tag = base
		if base != "" && script != "" {
			parsed, err := shared.ParseLocale(base)
			if err != nil {
				return ""
			}
			tag = shared.Locale{Language: parsed.Language, Script: script, Region: parsed.Region}.String()
		}
	}
	if tag == "" {
		tag = "en-US"
	}
	windows, err := shared.WindowsGameLocale(tag)
	if err != nil {
		return ""
	}
	return windows
}
