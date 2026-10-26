package keyring

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
	"xodus-nexus-wails/internal/shared"
)

type options struct {
	owned, locked bool
	alias         string
	names         []string
	enabled       string
	fail          bool
	provider      Provider
	config        map[string]string
	configFailure string
	collections   []string
}

type fake struct {
	mu         sync.Mutex
	opts       options
	calls      [][]string
	identifies int
	starts     []KDEProvider
}

func reply(kind string, value any) string {
	data, _ := json.Marshal(map[string]any{"type": kind, "data": []any{value}})
	return string(data)
}

func (f *fake) execute(command string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{command}, args...))
	if f.opts.fail {
		return "", errors.New("private provider text and identifiers")
	}
	if strings.HasPrefix(command, "kreadconfig") {
		if command == f.opts.configFailure || f.opts.configFailure == "all" {
			return "", errors.New("No KConfig client")
		}
		if value, ok := f.opts.config[args[3]+"/"+args[5]]; ok {
			return value, nil
		}
		if f.opts.enabled != "" {
			return f.opts.enabled, nil
		}
		return "UNSET", nil
	}
	has := func(name string) bool { return slices.Contains(args, name) }
	switch {
	case has("NameHasOwner"):
		return reply("b", f.opts.owned), nil
	case has("ListActivatableNames"):
		names := f.opts.names
		if names == nil {
			names = []string{"org.freedesktop.secrets"}
		}
		return reply("as", names), nil
	case has("ReadAlias"):
		alias := f.opts.alias
		if alias == "" {
			alias = "/org/freedesktop/secrets/collection/default"
		}
		return reply("o", alias), nil
	case has("Collections"):
		collections := f.opts.collections
		if collections == nil {
			collections = []string{}
		}
		return reply("v", map[string]any{"type": "ao", "data": collections}), nil
	case has("Get"):
		return reply("v", map[string]any{"type": "b", "data": f.opts.locked}), nil
	case has("StartServiceByName"):
		f.opts.owned = true
		return reply("u", 1), nil
	}
	return "", errors.New("Unexpected command")
}

func (f *fake) count(match func([]string) bool) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if match(call) {
			n++
		}
	}
	return n
}

func activations(call []string) bool { return slices.Contains(call, "StartServiceByName") }
func kconfig(call []string) bool     { return strings.HasPrefix(call[0], "kreadconfig") }

func fixture(o options, desktop string, actions Actions) (*fake, *Support) {
	f := &fake{opts: o}
	if actions.StartProvider == nil {
		actions.StartProvider = func(provider KDEProvider, _ launcherenv.Env, _ func() bool) error {
			f.mu.Lock()
			f.starts = append(f.starts, provider)
			f.opts.owned = true
			f.mu.Unlock()
			return nil
		}
	}
	identify := func() Provider {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.identifies++
		if f.opts.provider == "" {
			return Unknown
		}
		return f.opts.provider
	}
	return f, NewSupport(launcherenv.Env{"XDG_CURRENT_DESKTOP": desktop, "DISPLAY": ":1"}, f.execute, identify, actions)
}

func TestAutomaticStartup(t *testing.T) {
	for _, provider := range []Provider{KWallet, KSecretD} {
		f, support := fixture(options{provider: provider}, "gamescope", Actions{})
		if support.StartForStartup().State != "ready" || len(f.starts) != 1 {
			t.Errorf("%s: auto start", provider)
		}
	}
	for _, provider := range []Provider{Other, Unknown} {
		f, support := fixture(options{provider: provider}, "gamescope", Actions{})
		if support.StartForStartup().State != "stopped" || len(f.starts) != 0 || f.count(activations) != 0 {
			t.Errorf("%s activated automatically", provider)
		}
		if support.Start().State != "ready" {
			t.Errorf("%s explicit start", provider)
		}
	}
	for _, o := range []options{{enabled: "false"}, {config: map[string]string{"org.freedesktop.secrets/apiEnabled": "false"}}, {configFailure: "all"}} {
		o.provider = KWallet
		f, support := fixture(o, "gamescope", Actions{})
		if support.StartForStartup().State == "ready" || len(f.starts) != 0 {
			t.Errorf("%+v activated", o)
		}
	}
	unlocks := 0
	_, desktop := fixture(options{owned: true, locked: true}, "KDE", Actions{CanUnlock: func() bool { return true }, Unlock: func() string { unlocks++; return "ready" }})
	if desktop.StartForStartup().State != "locked" || unlocks != 0 {
		t.Error("Desktop startup must stay metadata-only")
	}
}

func TestUnlockResults(t *testing.T) {
	for _, result := range []string{"ready", "cancelled", "timed-out", "unavailable"} {
		var f *fake
		unlocks := 0
		actions := Actions{CanUnlock: func() bool { return true }, Unlock: func() string {
			unlocks++
			if result == "ready" {
				f.mu.Lock()
				f.opts.locked = false
				f.mu.Unlock()
			}
			return result
		}}
		var support *Support
		f, support = fixture(options{owned: true, locked: true}, "gamescope", actions)
		if status := support.Check(); status.State != "locked" || !status.CanUnlock || unlocks != 0 {
			t.Errorf("%s: check %+v", result, status)
		}
		want := "locked"
		if result == "ready" {
			want = "ready"
		}
		if status := support.Start(); status.State != want || unlocks != 1 || len(f.starts) != 0 || f.identifies != 0 {
			t.Errorf("%s: start %+v", result, status)
		}
	}
}

func TestCoalescedUnlock(t *testing.T) {
	release := make(chan string)
	var calls int
	var mu sync.Mutex
	_, support := fixture(options{owned: true, locked: true}, "", Actions{CanUnlock: func() bool { return true }, Unlock: func() string {
		mu.Lock()
		calls++
		mu.Unlock()
		return <-release
	}})
	results := make(chan shared.KeyringStatus, 3)
	for range 2 {
		go func() { results <- support.Start() }()
	}
	for !support.Busy() {
		time.Sleep(time.Millisecond)
	}
	go func() { results <- support.Check() }()
	time.Sleep(20 * time.Millisecond)
	release <- "cancelled"
	for range 3 {
		<-results
	}
	if calls != 1 || support.Busy() || support.Check().State != "locked" {
		t.Errorf("calls = %d", calls)
	}
}

func TestActivationChoices(t *testing.T) {
	f, support := fixture(options{provider: KSecretD}, "KDE", Actions{})
	if support.Start().State != "ready" || len(f.starts) != 0 || f.count(activations) != 1 {
		t.Error("Desktop activation must use D-Bus")
	}
	for _, c := range []struct {
		provider Provider
		names    []string
	}{{KWallet, []string{"org.freedesktop.secrets"}}, {KSecretD, []string{"org.freedesktop.secrets"}},
		{KWallet, []string{"org.freedesktop.secrets", "org.kde.kwalletd6"}}, {KSecretD, []string{"org.freedesktop.secrets", "org.kde.secretservicecompat"}}} {
		f, support := fixture(options{provider: c.provider, names: c.names, enabled: "false"}, "gamescope", Actions{})
		if status := support.Check(); status.State != "disabled" || status.CanStart {
			t.Errorf("%v: %+v", c, status)
		}
		if support.Start().State != "disabled" || f.identifies == 0 || f.count(activations) != 0 {
			t.Errorf("%v: start", c)
		}
	}
	for _, provider := range []Provider{Other, Unknown} {
		f, support := fixture(options{provider: provider, names: []string{"org.freedesktop.secrets", "org.kde.kwalletd6"}, enabled: "false"}, "gamescope", Actions{})
		if status := support.Start(); status.State != "ready" || f.count(kconfig) != 0 {
			t.Errorf("%s blocked by unrelated settings", provider)
		}
	}
	for _, locked := range []bool{true, false} {
		f, support := fixture(options{owned: true, locked: locked, provider: KWallet, enabled: "false"}, "gamescope", Actions{})
		want := map[bool]string{true: "locked", false: "ready"}[locked]
		if support.Start().State != want || f.identifies != 0 || f.count(func(c []string) bool { return kconfig(c) || activations(c) }) != 0 {
			t.Errorf("existing wallet locked=%v", locked)
		}
	}
	for _, provider := range []Provider{KWallet, KSecretD} {
		f, support := fixture(options{provider: provider, config: map[string]string{"org.freedesktop.secrets/apiEnabled": "false"}}, "gamescope", Actions{})
		if support.Start().State != "disabled" {
			t.Errorf("%s API flag ignored", provider)
		}
		var groups []string
		for _, call := range f.calls {
			if kconfig(call) {
				groups = append(groups, call[4])
			}
		}
		want := map[Provider]string{KWallet: "Wallet", KSecretD: "KSecretD"}[provider]
		if !slices.Equal(groups, []string{want, "org.freedesktop.secrets"}) {
			t.Errorf("%s groups = %v", provider, groups)
		}
	}
	if _, support := fixture(options{provider: KSecretD, config: map[string]string{"Wallet/Enabled": "false"}}, "gamescope", Actions{}); support.Start().State != "ready" {
		t.Error("legacy Wallet flag applied to ksecretd")
	}
	f, support = fixture(options{provider: KWallet, enabled: "false", configFailure: "kreadconfig6"}, "gamescope", Actions{})
	if support.Start().State != "disabled" || f.count(func(c []string) bool { return c[0] == "kreadconfig5" }) == 0 {
		t.Error("kreadconfig5 fallback")
	}
	if _, support := fixture(options{provider: KWallet, configFailure: "all"}, "gamescope", Actions{}); support.Start().State != "ready" {
		t.Error("missing KConfig clients must not block activation")
	}
}

func TestMetadataOnlyChecks(t *testing.T) {
	f, support := fixture(options{owned: true}, "gamescope", Actions{})
	status := support.Check()
	commands := fmt.Sprint(f.calls)
	if status.State != "ready" || status.CanStart || !status.GameMode || !strings.Contains(commands, "--auto-start=no") ||
		!strings.Contains(commands, "--allow-interactive-authorization=no") || strings.Contains(commands, "StartServiceByName") {
		t.Errorf("status %+v commands %s", status, commands)
	}
	if _, support := fixture(options{owned: true, alias: "/"}, "", Actions{}); support.Check().State != "needs-setup" {
		t.Error("no default collection")
	}
	f, support = fixture(options{locked: true}, "", Actions{})
	if support.Check().State != "stopped" || f.count(activations) != 0 || support.Start().State != "locked" || f.count(activations) != 1 {
		t.Error("activation alone is not ready")
	}
	for _, name := range []string{"org.freedesktop.secrets", "org.kde.secretservicecompat", "org.kde.kwalletd6", "org.kde.kwalletd5"} {
		f, support := fixture(options{names: []string{name}}, "gamescope", Actions{})
		if support.Start().State != "ready" {
			t.Errorf("%s not started", name)
		}
		if name == "org.freedesktop.secrets" {
			if len(f.starts) != 0 || f.count(activations) != 1 {
				t.Errorf("%s: wrong path", name)
			}
		} else {
			want := map[string]KDEProvider{"org.kde.secretservicecompat": ProviderKSecretD, "org.kde.kwalletd6": ProviderKWallet, "org.kde.kwalletd5": ProviderKWallet5}[name]
			if !slices.Equal(f.starts, []KDEProvider{want}) || f.count(activations) != 0 {
				t.Errorf("%s: starts %v", name, f.starts)
			}
		}
	}
	if _, support := fixture(options{names: []string{"unrelated.service"}}, "", Actions{}); support.Start().State != "missing" {
		t.Error("unadvertised provider")
	}
	for _, execute := range []Execute{
		func(string, ...string) (string, error) { return "", errors.New("secret private path") },
		func(string, ...string) (string, error) { return "not JSON", nil },
		func(string, ...string) (string, error) { return `{"type":"b","data":["yes"]}`, nil },
	} {
		support := NewSupport(launcherenv.Env{}, execute, func() Provider { return Unknown }, Actions{})
		if status := support.Start(); status.State != "unavailable" || status.CanStart || strings.Contains(support.Check().Message, "secret private") {
			t.Errorf("status = %+v", status)
		}
	}
}

func TestCreateCollection(t *testing.T) {
	setup := func(o options, capability bool, create func() string) (*fake, *Support) {
		o.owned, o.alias = !o.owned, "/"
		if o.names == nil {
			o.names = []string{}
		}
		return fixture(o, "gamescope", Actions{CanCreate: func() bool { return capability }, Create: create})
	}
	creates := 0
	f, support := setup(options{}, true, func() string { creates++; return "cancelled" })
	for _, status := range []shared.KeyringStatus{support.Check(), support.StartForStartup(), support.Start()} {
		if status.State != "needs-setup" || !status.CanCreate {
			t.Errorf("status = %+v", status)
		}
	}
	if creates != 0 || strings.Contains(fmt.Sprint(f.calls), "CreateCollection") {
		t.Error("created without a click")
	}
	for i, o := range []struct {
		opts       options
		capability bool
	}{{options{collections: []string{"/collection/existing"}}, true}, {options{}, false}, {options{owned: true}, true}} {
		creates := 0
		_, support := setup(o.opts, o.capability, func() string { creates++; return "ready" })
		support.CreateCollection()
		if support.Check().CanCreate || creates != 0 {
			t.Errorf("case %d offered creation", i)
		}
	}
	for _, result := range []string{"cancelled", "timed-out", "unavailable", "ready"} {
		_, support := setup(options{}, true, func() string { return result })
		after := support.CreateCollection()
		if after.State != "needs-setup" || !after.CanCreate {
			t.Errorf("%s: %+v", result, after)
		}
	}
	var fk *fake
	fk, support = setup(options{}, true, func() string {
		fk.mu.Lock()
		fk.opts.alias = "/collection/native"
		fk.mu.Unlock()
		return "ready"
	})
	if support.CreateCollection().State != "ready" {
		t.Error("native wallet")
	}
}

func service(exec, name string) string {
	return "[D-BUS Service]\nName=" + name + "\nExec=" + exec + "\n"
}

func TestActivationProvider(t *testing.T) {
	for exec, want := range map[string]Provider{"/usr/bin/kwalletd5": KWallet, "/usr/bin/kwalletd6": KWallet, "/usr/lib/ksecretd": KSecretD,
		`"/path with spaces/ksecretd" --arg`: KSecretD, "/usr/bin/gnome-keyring-daemon --start --components=secrets": Other,
		"/usr/bin/keepassxc": Other, "/usr/bin/env ksecretd": Other, "/usr/bin/not-kwalletd6": Other, "/usr/bin/ksecretd-helper": Other,
		"$(/usr/bin/ksecretd)": Other, `"/usr/bin/ksecretd"; touch /tmp/no`: Unknown} {
		if name, got := ActivationProvider(service(exec, serviceName)); name != serviceName || got != want {
			t.Errorf("%s = %s", exec, got)
		}
	}
	if _, got := ActivationProvider(" # comment\r\n[D-BUS Service]\r\n Name = org.freedesktop.secrets \r\n Exec = /usr/bin/ksecretd \r\n[Other]\r\nExec=/usr/bin/kwalletd6\r\n"); got != KSecretD {
		t.Errorf("CRLF = %s", got)
	}
	base := service("/usr/bin/kwalletd6", serviceName)
	for _, text := range []string{base + "Exec=/usr/bin/gnome-keyring-daemon\n", base + "Name=another.service\n", "[D-BUS Service]\nName=org.freedesktop.secrets\n",
		base + "\x00", strings.Repeat("x", 16385), base + "SystemdService=some-other-provider.service\n"} {
		if _, got := ActivationProvider(text); got != Unknown {
			t.Errorf("%.40q = %s", text, got)
		}
	}
	if _, got := ActivationProvider(service("/usr/bin/ksecretd", serviceName) + "SystemdService=plasma-ksecretd.service\n"); got != KSecretD {
		t.Error("KDE unit")
	}
}

type providerFixture struct {
	root, builtin string
	env           launcherenv.Env
}

func newProviderFixture(t *testing.T) providerFixture {
	root := t.TempDir()
	return providerFixture{root: root, builtin: filepath.Join(root, "builtin"), env: launcherenv.Env{"HOME": root, "XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
		"XDG_DATA_HOME": filepath.Join(root, "user"), "XDG_DATA_DIRS": filepath.Join(root, "local") + ":" + filepath.Join(root, "system")}}
}

func (p providerFixture) write(base, name, text string) string {
	dir := filepath.Join(base, "dbus-1/services")
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, name)
	os.WriteFile(file, []byte(text), 0o644)
	return file
}

func (p providerFixture) lookup() Provider { return SecretServiceProvider(p.env, p.builtin) }

func TestSecretServiceProvider(t *testing.T) {
	for binary, want := range map[string]Provider{"kwalletd5": KWallet, "kwalletd6": KWallet, "ksecretd": KSecretD} {
		p := newProviderFixture(t)
		p.write(p.env["XDG_DATA_HOME"], "org.freedesktop.secrets.service", service("/usr/bin/"+binary, serviceName))
		if p.lookup() != want {
			t.Errorf("%s", binary)
		}
	}
	p := newProviderFixture(t)
	target := p.write(p.builtin, "org.freedesktop.secrets.service", service("/usr/bin/kwalletd6", serviceName))
	p.write(p.env["XDG_DATA_HOME"], "gnome-keyring.service", service("/usr/bin/gnome-keyring-daemon", serviceName))
	if p.lookup() != Other {
		t.Error("user directory precedence")
	}
	runtime := filepath.Join(p.env["XDG_RUNTIME_DIR"], "dbus-1/services")
	os.MkdirAll(runtime, 0o755)
	os.Symlink(target, filepath.Join(runtime, "org.freedesktop.secrets.service"))
	if p.lookup() != KWallet {
		t.Error("runtime precedence through a symlink")
	}
	p = newProviderFixture(t)
	p.write(p.env["XDG_DATA_HOME"], "org.freedesktop.secrets.service", service("/usr/bin/kwalletd6", serviceName))
	p.write(p.env["XDG_DATA_HOME"], "gnome.service", service("/usr/bin/gnome-keyring-daemon", serviceName))
	if p.lookup() != Unknown {
		t.Error("duplicate registrations")
	}
	p = newProviderFixture(t)
	p.write(p.env["XDG_DATA_HOME"], "org.kde.kwalletd6.service", service("/usr/bin/kwalletd6", "org.kde.kwalletd6"))
	if p.lookup() != Unknown {
		t.Error("KDE-specific activation")
	}
	p = newProviderFixture(t)
	p.write(p.env["XDG_RUNTIME_DIR"], "renamed.service", service("/usr/bin/kwalletd6", serviceName))
	if p.lookup() != Unknown {
		t.Error("noncanonical runtime name")
	}
	for _, invalid := range []string{"directory", "oversized", "encoding"} {
		p := newProviderFixture(t)
		p.write(p.builtin, "org.freedesktop.secrets.service", service("/usr/bin/kwalletd6", serviceName))
		file := p.write(p.env["XDG_DATA_HOME"], "org.freedesktop.secrets.service", service("/usr/bin/kwalletd6", serviceName))
		switch invalid {
		case "directory":
			os.Remove(file)
			os.Mkdir(file, 0o755)
		case "oversized":
			os.WriteFile(file, []byte(strings.Repeat("x", 16385)), 0o644)
		case "encoding":
			os.WriteFile(file, []byte{0xff, 0xfe}, 0o644)
		}
		if p.lookup() != Unknown {
			t.Errorf("%s fell through", invalid)
		}
	}
	p = newProviderFixture(t)
	if p.lookup() != Unknown || SecretServiceProvider(launcherenv.Env{"HOME": p.root, "XDG_DATA_HOME": "relative", "XDG_DATA_DIRS": "also-relative"}, p.builtin) != Unknown {
		t.Error("missing/relative paths")
	}
	many := p.env.Clone()
	many["XDG_DATA_DIRS"] = strings.TrimSuffix(strings.Repeat(p.root+":", 40), ":")
	if SecretServiceProvider(many, p.builtin) != Unknown {
		t.Error("excessive search paths")
	}
}

func helper(t *testing.T, script string) string {
	file := filepath.Join(t.TempDir(), "backend")
	os.WriteFile(file, []byte("#!/bin/sh\n"+script), 0o700)
	return file
}

func TestUnlocker(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	binary := helper(t, `printf '%s\n' "$@" > "`+record+`"; printf '{"schema":1,"state":"ready"}'`)
	var unlocker Unlocker
	if unlocker.Create(binary, "12345") != "ready" {
		t.Error("create")
	}
	if data, _ := os.ReadFile(record); string(data) != "keyring-create\n--window-id\n12345\n" {
		t.Errorf("args = %q", data)
	}
	for _, state := range walletStates {
		if got := unlocker.Unlock(helper(t, `printf '{"schema":1,"state":"`+state+`"}'`), "12345"); got != state {
			t.Errorf("%s = %s", state, got)
		}
	}
	for _, output := range []string{"private provider text", `{"schema":1,"state":"private-secret"}`, `{"schema":2,"state":"ready"}`, `{"state":"ready"}`} {
		if unlocker.Unlock(helper(t, "printf '%s' '"+output+"'"), "") != "unavailable" {
			t.Errorf("%s accepted", output)
		}
	}
	if unlocker.Unlock(binary, "1\n--bad") != "unavailable" {
		t.Error("unsafe window ID")
	}
	slow := helper(t, `trap 'printf "{\"schema\":1,\"state\":\"cancelled\"}"; exit 0' INT; while :; do sleep 0.05; done`)
	result := make(chan string)
	go func() { result <- unlocker.Unlock(slow, "") }()
	for !unlocker.Busy() {
		time.Sleep(time.Millisecond)
	}
	if unlocker.Unlock(binary, "") != "unavailable" || unlocker.Create(binary, "") != "unavailable" {
		t.Error("overlapping request accepted")
	}
	time.Sleep(50 * time.Millisecond)
	unlocker.Cancel()
	if got := <-result; got != "cancelled" || unlocker.Busy() {
		t.Errorf("cancel = %s", got)
	}
}
