package shared

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/language"
)

// GameLanguageOverrides are selectable overrides, not a claim that a package includes translations.
var GameLanguageOverrides = []string{
	"ar-SA", "bg-BG", "cs-CZ", "da-DK", "de-DE", "el-GR", "en-US", "en-GB",
	"es-ES", "es-MX", "fi-FI", "fr-FR", "he-IL", "hu-HU", "id-ID", "it-IT",
	"ja-JP", "ko-KR", "nb-NO", "nl-NL", "pl-PL", "pt-BR", "pt-PT", "ro-RO",
	"ru-RU", "sk-SK", "sv-SE", "th-TH", "tr-TR", "uk-UA", "vi-VN", "zh-CN", "zh-TW",
}

var localePattern = regexp.MustCompile(`(?i)^[a-z]{2,3}(?:-[a-z]{4})?(?:-(?:[a-z]{2}|[0-9]{3}))?$`)

// Locale is a parsed BCP 47 tag limited to language, script and region.
type Locale struct {
	Language, Script, Region string
}

func (l Locale) String() string {
	parts := []string{l.Language}
	if l.Script != "" {
		parts = append(parts, l.Script)
	}
	if l.Region != "" {
		parts = append(parts, l.Region)
	}
	return strings.Join(parts, "-")
}

// ParseLocale mirrors new Intl.Locale(tag) for language[-Script][-Region] tags.
func ParseLocale(tag string) (Locale, error) {
	parsed, err := language.Parse(tag)
	if err != nil {
		return Locale{}, err
	}
	base, _ := parsed.Base()
	locale := Locale{Language: base.String()}
	if script, confidence := parsed.Script(); confidence == language.Exact {
		locale.Script = script.String()
	}
	if region, confidence := parsed.Region(); confidence == language.Exact {
		locale.Region = region.String()
	}
	return locale, nil
}

// Maximize adds likely script and region subtags, like Intl.Locale.maximize.
func (l Locale) Maximize() Locale {
	parsed, err := language.Parse(l.String())
	if err != nil {
		return l
	}
	out := l
	if script, confidence := parsed.Script(); out.Script == "" && confidence != language.No {
		out.Script = script.String()
	}
	if region, confidence := parsed.Region(); out.Region == "" && confidence != language.No && region.String() != "ZZ" {
		out.Region = region.String()
	}
	return out
}

func NormalizeGameLocale(value string) (string, error) {
	tag := strings.TrimSpace(value)
	if len(tag) > 35 || !localePattern.MatchString(tag) {
		return "", errors.New("Enter a language code such as pl-PL, de or zh-Hant.")
	}
	first, _, _ := strings.Cut(tag, "-")
	locale, err := ParseLocale(tag)
	if err != nil || strings.EqualFold(first, "und") || locale.Language == "" || locale.Language == "und" {
		return "", errors.New("Invalid game language code.")
	}
	return locale.String(), nil
}

func WindowsGameLocale(value string) (string, error) {
	normalized, err := NormalizeGameLocale(value)
	if err != nil {
		return "", err
	}
	locale, _ := ParseLocale(normalized)
	expanded := locale.Maximize()
	if expanded.Region == "" {
		return "", errors.New("This language needs an explicit region code.")
	}
	if locale.Language == "zh" && locale.Script != "" {
		if regional, _ := ParseLocale("zh-" + expanded.Region); locale.Script != regional.Maximize().Script {
			return "", errors.New("The Chinese script and region must agree.")
		}
	}
	// Windows uses zh-CN / zh-TW. Other explicit scripts must remain distinct.
	script := locale.Script
	if script == "" && slices.Contains([]string{"sr", "az", "bs", "uz"}, locale.Language) {
		script = expanded.Script
	}
	if locale.Language == "zh" {
		script = ""
	}
	return Locale{locale.Language, script, expanded.Region}.String(), nil
}
