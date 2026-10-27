package manifest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

func mustParse(t *testing.T, xml string) *Node {
	t.Helper()
	root, err := Parse(xml)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestManifestLookup(t *testing.T) {
	root := mustParse(t, `<Game xmlns:g="urn:test"><g:shellvisuals
      defaultdisplayname='No Man&apos;s &quot;Sky&quot; > Space'/><g:STOREID>9AAAAAAAAAAA</g:STOREID></Game>`)
	if got := Attr(root, "ShellVisuals", "DefaultDisplayName"); got != `No Man's "Sky" > Space` {
		t.Errorf("attr = %q", got)
	}
	if Text(root, "StoreId") != "9AAAAAAAAAAA" {
		t.Error("namespaced text")
	}
	root = mustParse(t, `<Game><!-- <!DOCTYPE Example> <!ENTITY example "ignored"> -->
      <Description><![CDATA[<StoreId>000000000000</StoreId>]]></Description></Game>`)
	if Text(root, "StoreId") != "" {
		t.Error("CDATA markup must not be parsed")
	}
	if Text(mustParse(t, "<Game>\n<StoreId>9AAAAAAAAAAA</StoreId \t>\n</Game \n>"), "StoreId") != "9AAAAAAAAAAA" {
		t.Error("whitespace before >")
	}
	if Text(mustParse(t, "\ufeff<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<Game><StoreId>9AAAAAAAAAAA</StoreId></Game>"), "StoreId") != "9AAAAAAAAAAA" {
		t.Error("UTF-8 BOM before the XML declaration")
	}
	for _, xml := range []string{
		`<!DOCTYPE Game [<!ENTITY x SYSTEM "file:///secret">]><Game>&x;</Game>`,
		`<!DOCTYPE Game SYSTEM "https://example.invalid/no.xml"><Game/>`,
		`<Game><!-- unfinished <StoreId>000000000000</StoreId></Game>`,
		`<Game><!-- invalid -- comment --></Game>`,
		`<Game><Identity Name="unfinished/></Game>`,
	} {
		if _, err := Parse(xml); err == nil {
			t.Errorf("accepted %s", xml)
		}
	}
}

func TestResolvePackageFile(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin/gaming.desktop.x64"), 0o755)
	os.WriteFile(filepath.Join(root, "bin/gaming.desktop.x64/WITCHER3.EXE"), []byte("encrypted fixture"), 0o644)
	if got, ok := ResolvePackageFile(root, `bin\Gaming.Desktop.x64\witcher3.exe`); !ok || got != "bin/gaming.desktop.x64/WITCHER3.EXE" {
		t.Errorf("mixed case = %q %v", got, ok)
	}
	safe := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(safe, "Game.exe"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(safe, "GAME.EXE"), []byte("B"), 0o644)
	if got, _ := ResolvePackageFile(safe, "Game.exe"); got != "Game.exe" {
		t.Error("exact name must win")
	}
	for _, invalid := range []string{"game.exe", "../Game.exe", `C:\Game.exe`, ".", "missing.exe"} {
		if _, ok := ResolvePackageFile(safe, invalid); ok {
			t.Errorf("resolved %q", invalid)
		}
	}
	os.Mkdir(filepath.Join(safe, "directory.exe"), 0o755)
	os.WriteFile(filepath.Join(outside, "external.exe"), []byte("outside"), 0o644)
	os.Symlink(outside, filepath.Join(safe, "External"))
	os.Symlink("Game.exe", filepath.Join(safe, "alias.exe"))
	for _, invalid := range []string{"directory.exe", "external/EXTERNAL.exe"} {
		if _, ok := ResolvePackageFile(safe, invalid); ok {
			t.Errorf("resolved %q", invalid)
		}
	}
	if got, _ := ResolvePackageFile(safe, "ALIAS.EXE"); got != "alias.exe" {
		t.Errorf("alias = %q", got)
	}
}

func TestManifestLocalesAndTitle(t *testing.T) {
	xml := "<Game><!-- example -> \x1b[31m <TitleId>0000</TitleId><Resource Language=\"fr-FR\"/> -->\n" +
		"<TitleId>6BE4<!-- annotation -->1AB5</TitleId><Resources><Resource Language='en'/>\n" +
		"<!-- <Resource Language=\"de-DE\"/> --><Resource Language=\"cs-CZ\"/></Resources></Game>"
	if id, _ := TitleID(xml); id != "6BE41AB5" {
		t.Errorf("title = %q", id)
	}
	if locales, _ := Locales(xml); !slices.Equal(locales, []string{"en", "cs-CZ"}) {
		t.Errorf("locales = %v", locales)
	}
	if id, _ := TitleID("<Game><!-- <TitleId>0000</TitleId> --></Game>"); id != "" {
		t.Error("commented title")
	}
	if locales, _ := Locales(`<Game><![CDATA[<Resource Language="fr-FR"/>]]></Game>`); len(locales) != 0 {
		t.Error("CDATA locale")
	}
	if id, _ := TitleID("<TitleId> 6BE41AB5 </TitleId>"); id != "6BE41AB5" {
		t.Error("trimmed title")
	}
}

func TestSupportedGameLocale(t *testing.T) {
	declared, _ := Locales(`<Resources><Resource Language="en"/><Resource Language="cs-CZ"/><Resource Language="cs-CZ"/></Resources>`)
	cases := []struct {
		requested string
		locales   []string
		want      string
	}{{"cs-cz", declared, "cs-CZ"}, {"fr-FR", declared, "fr-FR"}, {"cs-CZ", []string{"en", "cs", "de"}, "cs-CZ"},
		{"de-DE", []string{"en", "cs", "de"}, "de-DE"}, {"pt-PT", []string{"pt-BR"}, "pt-PT"}, {"zh-Hant", []string{"zh-Hans"}, "zh-Hant"},
		{"pl-PL", []string{"en"}, "pl-PL"}, {"", nil, ""}}
	for _, c := range cases {
		if got, err := SupportedGameLocale(c.requested, c.locales); err != nil || got != c.want {
			t.Errorf("SupportedGameLocale(%q) = %q, %v", c.requested, got, err)
		}
	}
	for _, input := range []string{"../../pl", "de\nLANG=fr", "en_US", "ms-resource:Language", "en-x-private", "en-u-ca-gregory", "x", "123", "und", "und-Latn", "und-US", "zh-Hant-CN"} {
		if _, err := SupportedGameLocale(input, nil); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestGameLocaleEnvironment(t *testing.T) {
	original := launcherenv.Env{"LANG": "en_US.UTF-8", "LC_ALL": "C.UTF-8"}
	result, err := GameLocaleEnvironment(original, "cs-CZ")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"XODUS_PACKAGE_LOCALE": "cs-CZ", "WINEUSERLOCALE": "cs-CZ", "LANG": "cs_CZ.UTF-8", "LANGUAGE": "cs_CZ:cs"}
	for _, key := range localeVariables {
		want[key] = "cs_CZ.UTF-8"
	}
	for k, v := range want {
		if result[k] != v {
			t.Errorf("%s = %q, want %q", k, result[k], v)
		}
	}
	if original["LANG"] != "en_US.UTF-8" || len(original) != 2 {
		t.Error("caller environment mutated")
	}
	for _, c := range [][3]string{{"de", "de-DE", "de_DE.UTF-8"}, {"pl", "pl-PL", "pl_PL.UTF-8"}, {"pt-BR", "pt-BR", "pt_BR.UTF-8"},
		{"pt-PT", "pt-PT", "pt_PT.UTF-8"}, {"zh-Hans", "zh-CN", "zh_CN.UTF-8"}, {"zh-Hant", "zh-TW", "zh_TW.UTF-8"}, {"es-419", "es-419", "es_419.UTF-8"},
		{"sr-Latn", "sr-Latn-RS", "sr_RS.UTF-8@latin"}, {"sr-Cyrl", "sr-Cyrl-RS", "sr_RS.UTF-8@cyrillic"}} {
		if windows, err := shared.WindowsGameLocale(c[0]); err != nil || windows != c[1] {
			t.Errorf("WindowsGameLocale(%s) = %q, %v", c[0], windows, err)
		}
		env, err := GameLocaleEnvironment(launcherenv.Env{"LANG": "en_US.UTF-8"}, c[0])
		if err != nil || env["XODUS_PACKAGE_LOCALE"] != c[0] || env["WINEUSERLOCALE"] != c[1] || env["LANG"] != c[2] || env["LC_ALL"] != c[2] {
			t.Errorf("%s env = %v, %v", c[0], env, err)
		}
	}
	if normalized, _ := shared.NormalizeGameLocale(" PT-br "); normalized != "pt-BR" {
		t.Errorf("normalized = %q", normalized)
	}
	cleared, _ := GameLocaleEnvironment(launcherenv.Env{"LANG": "pl_PL.UTF-8", "XODUS_PACKAGE_LOCALE": "de", "WINEUSERLOCALE": "de-DE"}, "")
	if len(cleared) != 1 || cleared["LANG"] != "pl_PL.UTF-8" {
		t.Errorf("cleared = %v", cleared)
	}
	for env, want := range map[string]string{"C.UTF-8|pl_PL.UTF-8": "en-US", "|sr_RS.UTF-8@latin": "sr-Latn-RS", "|pl_PL.UTF-8": "pl-PL", "|unsafe/path": ""} {
		all, lang, _ := strings.Cut(env, "|")
		values := launcherenv.Env{"LANG": lang}
		if all != "" {
			values["LC_ALL"] = all
		}
		if got := SystemGameLocale(values); got != want {
			t.Errorf("SystemGameLocale(%v) = %q, want %q", values, got, want)
		}
	}
}

func TestInstalledManifests(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "MicrosoftGame.Config"), []byte(`<Game><TitleId>1234ABCD</TitleId><Resources><Resource Language="en"/><Resource Language="pl-PL"/></Resources></Game>`), 0o644)
	os.WriteFile(filepath.Join(root, "APPXMANIFEST.XML"), []byte(`<Package xmlns:r="urn:test"><Resources><r:Resource Language="PL-pl"/><r:Resource Language="de"/><r:Resource Language="ms-resource:Language"/></Resources></Package>`), 0o644)
	if locales, err := InstalledGameLocales(root); err != nil || !slices.Equal(locales, []string{"en", "pl-PL", "de"}) {
		t.Errorf("locales = %v, %v", locales, err)
	}
	if id, _ := InstalledGameTitleID(root); id != "1234ABCD" {
		t.Errorf("title = %q", id)
	}
	if locales, _ := InstalledGameLocales(t.TempDir()); len(locales) != 0 {
		t.Error("empty fixture")
	}

	bad := t.TempDir()
	file := filepath.Join(bad, "MicrosoftGame.config")
	os.WriteFile(file, []byte("<Game/>"), 0o644)
	os.WriteFile(filepath.Join(bad, "MicrosoftGame.Config"), []byte("<Game/>"), 0o644)
	if _, err := InstalledGameLocales(bad); err == nil || !strings.Contains(err.Error(), "Ambiguous") {
		t.Errorf("ambiguous: %v", err)
	}
	os.Remove(filepath.Join(bad, "MicrosoftGame.Config"))
	os.WriteFile(file, []byte(strings.Repeat("a", 1_048_577)), 0o644)
	if _, err := InstalledGameLocales(bad); err == nil || !strings.Contains(err.Error(), "Unsupported") {
		t.Errorf("oversized: %v", err)
	}
	os.WriteFile(file, []byte("<Game><Resources></Game>"), 0o644)
	if _, err := InstalledGameLocales(bad); err == nil {
		t.Error("malformed accepted")
	}
	os.Remove(file)
	os.Symlink(filepath.Join(t.TempDir(), "not-read"), file)
	if _, err := InstalledGameLocales(bad); err == nil {
		t.Error("symlink accepted")
	}
}
