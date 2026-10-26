// Package keyring ports keyring-provider.ts, keyring-session.ts, keyring.ts and
// keyring-unlock.ts: metadata-only checks and explicit preparation of the
// desktop Secret Service.
package keyring

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"xodus-nexus-wails/internal/launcherenv"
)

type Provider string

const (
	KWallet  Provider = "kwallet"
	KSecretD Provider = "ksecretd"
	Other    Provider = "other"
	Unknown  Provider = "unknown"
)

const (
	serviceName      = "org.freedesktop.secrets"
	maxActivationLen = 16_384
)

var (
	activationLine = regexp.MustCompile(`^([A-Za-z]+)\s*=(.*)$`)
	execArgument   = regexp.MustCompile(`^(?:"([^"\\]+)"|([^\s"'\\]+))(?:\s|$)`)
	kwalletBinary  = regexp.MustCompile(`^kwalletd[56]$`)
	lineBreak      = regexp.MustCompile(`\r?\n`)
)

// ActivationProvider reads activation metadata only. Exec is identified, never
// run or evaluated.
func ActivationProvider(text string) (name string, provider Provider) {
	if len(text) > maxActivationLen || strings.ContainsRune(text, 0) {
		return "", Unknown
	}
	section, duplicate := "", false
	values := map[string]string{}
	for _, raw := range lineBreak.Split(text, -1) {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "[D-BUS Service]" {
			continue
		}
		match := activationLine.FindStringSubmatch(line)
		if match == nil || !slices.Contains([]string{"Name", "Exec", "SystemdService"}, match[1]) {
			continue
		}
		if _, seen := values[match[1]]; seen {
			duplicate = true
		}
		values[match[1]] = strings.TrimSpace(match[2])
	}
	name = values["Name"]
	if duplicate || values["Exec"] == "" {
		return name, Unknown
	}
	// D-Bus Exec is not a shell command. Recognize only an ordinary first
	// executable argument, not wrappers, substitutions or compound commands.
	executable := execArgument.FindStringSubmatch(values["Exec"])
	if executable == nil {
		return name, Unknown
	}
	path := executable[1]
	if path == "" {
		path = executable[2]
	}
	binary := filepath.Base(path)
	provider = Other
	var units []string
	switch {
	case binary == "ksecretd":
		provider, units = KSecretD, []string{"plasma-ksecretd.service"}
	case kwalletBinary.MatchString(binary):
		provider, units = KWallet, []string{"plasma-kwalletd5.service", "plasma-kwalletd6.service"}
	}
	// An explicit systemd unit can override Exec. Do not attribute an unknown
	// unit to KDE based on an otherwise familiar executable.
	if unit := values["SystemdService"]; unit != "" && !slices.Contains(units, unit) {
		return name, Unknown
	}
	return name, provider
}

func readActivation(file string) (string, error) {
	// Symlinks to a distribution's provider-specific service file are valid.
	// Nonblocking open and fstat keep FIFOs/devices/directories out of this read.
	handle, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxActivationLen {
		return "", errors.New("Invalid activation metadata")
	}
	data, err := io.ReadAll(io.LimitReader(handle, maxActivationLen+1))
	if err != nil || len(data) > maxActivationLen || !utf8.Valid(data) {
		return "", errors.New("Invalid activation metadata")
	}
	return string(data), nil
}

// SecretServiceProvider attributes an unowned, activatable standard Secret
// Service on a best-effort basis, following session-service directory
// precedence. A running bus may use custom directories or cached metadata, so
// unknown/ambiguous results must not block a provider or infer KDE from the
// desktop or other installed services.
func SecretServiceProvider(env launcherenv.Env, systemDataDirectory string) Provider {
	if systemDataDirectory == "" {
		systemDataDirectory = "/usr/share"
	}
	dataHome := env["XDG_DATA_HOME"]
	if dataHome == "" {
		home := env["HOME"]
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		dataHome = filepath.Join(home, ".local/share")
	}
	dataDirs := env["XDG_DATA_DIRS"]
	if dataDirs == "" {
		dataDirs = "/usr/local/share:/usr/share"
	}
	bases := append(append([]string{env["XDG_RUNTIME_DIR"], dataHome}, strings.Split(dataDirs, ":")...), systemDataDirectory)
	if len(bases) > 32 {
		return Unknown
	}
	seen := map[string]bool{}
	for _, base := range bases {
		if base == "" || !filepath.IsAbs(base) {
			continue
		}
		directory := filepath.Join(base, "dbus-1/services")
		if seen[directory] {
			continue
		}
		seen[directory] = true
		runtime := base == env["XDG_RUNTIME_DIR"]
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || len(entries) > 1024 {
			return Unknown
		}
		var match *Provider
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".service") || (runtime && entry.Name() != serviceName+".service") {
				continue
			}
			text, err := readActivation(filepath.Join(directory, entry.Name()))
			if err != nil {
				return Unknown
			}
			name, provider := ActivationProvider(text)
			if name != serviceName {
				continue
			}
			// D-Bus makes no selection guarantee for duplicate names in one directory.
			if match != nil {
				return Unknown
			}
			match = &provider
		}
		if match != nil {
			return *match
		}
	}
	return Unknown
}
