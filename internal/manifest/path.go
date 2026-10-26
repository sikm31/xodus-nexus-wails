package manifest

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var windowsSeparators = regexp.MustCompile(`[\\/]+`)

// foldASCII lowercases A-Z only, like the TypeScript fold.
func foldASCII(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, value)
}

// ResolvePackageFile resolves a Windows package path without guessing among
// ambiguous Linux files. It returns the path relative to root, or ok=false.
func ResolvePackageFile(root, name string) (string, bool) {
	var parts []string
	for _, part := range windowsSeparators.Split(name, -1) {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 || slices.ContainsFunc(parts, func(part string) bool {
		return part == "." || part == ".." || strings.Contains(part, ":") || strings.Contains(part, "\x00")
	}) {
		return "", false
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	current := base
	for _, part := range parts {
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", false
		}
		var matches []string
		for _, entry := range entries {
			if entry.Name() == part {
				matches = []string{part}
				break
			}
			if foldASCII(entry.Name()) == foldASCII(part) {
				matches = append(matches, entry.Name())
			}
		}
		if len(matches) != 1 {
			return "", false
		}
		current = filepath.Join(current, matches[0])
		real, err := filepath.EvalSymlinks(current)
		if err != nil {
			return "", false
		}
		relative, err := filepath.Rel(base, real)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return "", false
		}
	}
	if info, err := os.Stat(current); err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	relative, err := filepath.Rel(base, current)
	return relative, err == nil
}
