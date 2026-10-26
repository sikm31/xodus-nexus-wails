package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/launcherenv"
)

func frameLine(value any) string {
	data, _ := json.Marshal(value)
	return Prefix + string(data)
}

func TestProtocol(t *testing.T) {
	for _, url := range []string{"https://login.live.com/ppsecure/InlineLogin.srf", "https://account.live.com/consent"} {
		if !TrustedURL(url) {
			t.Errorf("%s rejected", url)
		}
	}
	for _, url := range []string{"http://login.live.com/", "https://login.live.com.evil.invalid/", "https://user:secret@login.live.com/", "file:///tmp/test",
		"https://localhost/", "https://login.live.com:9443/", "javascript:alert(1)"} {
		if TrustedURL(url) {
			t.Errorf("%s accepted", url)
		}
	}
	open := map[string]any{"type": "open", "session": 1, "url": "https://login.live.com/", "headers": map[string]any{"hostapp": "CloudExperienceHost"}}
	if frame, err := ParseFrame(frameLine(open)); err != nil || frame.Type != Open {
		t.Errorf("open = %+v, %v", frame, err)
	}
	for _, headers := range []map[string]any{{"cookie": "secret"}, {"hostapp": "a\r\nCookie: secret"}, {"HostApp": "one", "hostapp": "two"}} {
		open["headers"] = headers
		if _, err := ParseFrame(frameLine(open)); err == nil || !strings.Contains(err.Error(), "headers") {
			t.Errorf("headers %v: %v", headers, err)
		}
	}
	if frame, err := ParseFrame("ordinary backend output"); frame != nil || err != nil {
		t.Error("ordinary output")
	}
	for _, line := range []string{Prefix + "secret-not-json", frameLine(map[string]any{"type": "ready", "version": 9, "secret": "private"}),
		frameLine(map[string]any{"type": "close", "session": -1}), frameLine(map[string]any{"type": "execute", "session": 1, "code": "secret"}),
		Prefix + strings.Repeat("x", FrameLimit)} {
		_, err := ParseFrame(line)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Errorf("%.40q: %v", line, err)
		}
	}
}

const fakeBackend = `#!/bin/sh
out() { printf 'NEXUS_AUTH_V1 %s\n' "$1"; }
case "$FIXTURE_MODE" in
  no-handshake) echo 'ordinary help'; exit 0;;
  credential-failure) printf 'synthetic-secret-must-not-be-logged\nNEXUS_CREDENTIAL_ERROR_V1 keyring-write-verification\n' >&2; exit 1;;
  unknown-credential) printf 'NEXUS_CREDENTIAL_ERROR_V1 synthetic-secret-must-not-be-logged\n' >&2; exit 1;;
esac
out '{"type":"ready","version":1}'
while IFS= read -r line; do
  case "$line" in
    *'"hello"'*)
      if [ "$FIXTURE_MODE" = invalid ]; then out '{"type":"execute","session":1,"code":"synthetic-secret"}'; continue; fi
      out '{"type":"open","session":1,"url":"https://login.live.com/ppsecure/InlineLogin.srf","headers":{"hostapp":"CloudExperienceHost"}}';;
    *'"ipc"'*)
      echo 'synthetic-secret-must-not-be-logged'
      [ "$FIXTURE_LATE" = 1 ] || out '{"type":"close","session":1}'
      sleep 0.02; exit 0;;
    *'"closed"'*) exit 1;;
  esac
done
`

type fakeView struct {
	open    func(Frame) error
	opens   atomic.Int32
	closes  atomic.Int32
	dispose atomic.Bool
}

func (v *fakeView) Open(frame Frame) error      { v.opens.Add(1); return v.open(frame) }
func (v *fakeView) Close(int64)                 { v.closes.Add(1) }
func (v *fakeView) Context(int64, string) error { return nil }
func (v *fakeView) Dispose()                    { v.dispose.Store(true) }

func backend(t *testing.T) string {
	binary := filepath.Join(t.TempDir(), "cli")
	os.WriteFile(binary, []byte(fakeBackend), 0o755)
	return binary
}

func env(values ...string) launcherenv.Env {
	e := launcherenv.FromOS()
	for i := 0; i+1 < len(values); i += 2 {
		e[values[i]] = values[i+1]
	}
	return e
}

func TestTransport(t *testing.T) {
	for _, mode := range []string{"success", "invalid", "no-handshake", "cancel", "load-failure", "credential-failure", "unknown-credential"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			log := diagnostics.NewBackend(root, nil, 0)
			diagnostics.SetBackend(log)
			defer diagnostics.SetBackend(nil)
			view := &fakeView{}
			var callbacks Callbacks
			view.open = func(frame Frame) error {
				switch mode {
				case "cancel":
					go callbacks.Cancel(frame.Session)
				case "load-failure":
					return errors.New("synthetic-secret")
				default:
					go callbacks.Send(frame.Session, `{"synthetic":"not-a-real-token"}`)
				}
				return nil
			}
			login, err := Start(backend(t), func(c Callbacks) View { callbacks = c; return view }, env("FIXTURE_MODE", mode), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			code, err := login.Wait()
			switch mode {
			case "credential-failure":
				if err == nil || !strings.Contains(err.Error(), "keyring did not return the credentials just saved") || view.opens.Load() != 0 {
					t.Errorf("err = %v", err)
				}
			case "invalid", "no-handshake", "load-failure", "unknown-credential":
				if err == nil || !regexp.MustCompile(`(?i)sign-in`).MatchString(err.Error()) {
					t.Errorf("err = %v", err)
				}
			default:
				want := 0
				if mode == "cancel" {
					want = 1
				}
				if err != nil || code != want {
					t.Errorf("code = %d, %v", code, err)
				}
			}
			if !view.dispose.Load() {
				t.Error("view not disposed")
			}
			logged, _ := log.Read()
			if !strings.Contains(logged.Content, `"operation":"login"`) || regexp.MustCompile(`synthetic-secret|login.live.com|CloudExperienceHost|not-a-real-token`).MatchString(logged.Content) {
				t.Errorf("log = %s", logged.Content)
			}
			if mode == "success" && (view.opens.Load() != 1 || view.closes.Load() != 1) {
				t.Errorf("opens %d closes %d", view.opens.Load(), view.closes.Load())
			}
		})
	}
}

func TestTransportCompletion(t *testing.T) {
	for _, late := range []bool{false, true} {
		abort := make(chan struct{})
		view := &fakeView{}
		var callbacks Callbacks
		view.open = func(frame Frame) error {
			callbacks.Send(frame.Session, "{}")
			<-abort
			return errors.New("load aborted by disposal")
		}
		disposing := &disposeHook{View: view, onDispose: func() { close(abort) }}
		flag := "0"
		if late {
			flag = "1"
		}
		login, _ := Start(backend(t), func(c Callbacks) View { callbacks = c; return disposing }, env("FIXTURE_MODE", "success", "FIXTURE_LATE", flag), 3*time.Second)
		code, err := login.Wait()
		if late {
			if err == nil || !strings.Contains(err.Error(), "did not complete its protocol") {
				t.Errorf("late: %v", err)
			}
		} else if err != nil || code != 0 {
			t.Errorf("completion: %d %v", code, err)
		}
	}
}

type disposeHook struct {
	View
	onDispose func()
	once      sync.Once
}

func (d *disposeHook) Dispose() { d.once.Do(d.onDispose); d.View.Dispose() }

type recordingView struct {
	name   string
	events *[]string
	mu     *sync.Mutex
	opens  []Frame
	ctx    []string
	gate   chan struct{}
}

func (r *recordingView) record(event string) {
	r.mu.Lock()
	*r.events = append(*r.events, event)
	r.mu.Unlock()
}
func (r *recordingView) Open(frame Frame) error {
	r.mu.Lock()
	r.opens = append(r.opens, frame)
	r.mu.Unlock()
	r.record(r.name + "-open")
	return nil
}
func (r *recordingView) Close(int64) {}
func (r *recordingView) Context(_ int64, c string) error {
	r.mu.Lock()
	r.ctx = append(r.ctx, c)
	r.mu.Unlock()
	return nil
}
func (r *recordingView) Dispose() {
	if r.gate != nil {
		<-r.gate
	}
	if r.name == "old" {
		r.record("old-disposed")
	}
}

type fallbackFixture struct {
	view              View
	request           func() error
	oldCallbacks      Callbacks
	embedded, browser *recordingView
	events            []string
	mu                sync.Mutex
	sends, cancels    atomic.Int32
	resolve           func() (ViewFactory, error)
}

func newFallback(resolve func(f *fallbackFixture) (ViewFactory, error)) *fallbackFixture {
	f := &fallbackFixture{}
	f.embedded = &recordingView{name: "old", events: &f.events, mu: &f.mu}
	f.browser = &recordingView{name: "browser", events: &f.events, mu: &f.mu}
	factory := func(Callbacks) View {
		f.mu.Lock()
		f.events = append(f.events, "browser-created")
		f.mu.Unlock()
		return f.browser
	}
	browser := func() (ViewFactory, error) { return factory, nil }
	if resolve != nil {
		browser = func() (ViewFactory, error) { return resolve(f) }
	}
	f.view = FallbackViewFactory(func(request func() error) ViewFactory {
		f.request = request
		return func(c Callbacks) View { f.oldCallbacks = c; return f.embedded }
	}, browser)(Callbacks{Send: func(int64, string) { f.sends.Add(1) }, Cancel: func(int64) { f.cancels.Add(1) }, Fail: func() {}})
	return f
}

var openFrame = Frame{Type: Open, Session: 1, URL: "https://login.live.com/", Headers: map[string]string{}}

func TestFallback(t *testing.T) {
	f := newFallback(nil)
	f.view.Open(openFrame)
	if err := f.request(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.events, []string{"old-open", "old-disposed", "browser-created", "browser-open"}) || len(f.browser.opens) != 1 {
		t.Errorf("events = %v", f.events)
	}
	f.oldCallbacks.Send(1, "stale")
	f.oldCallbacks.Cancel(1)
	if f.sends.Load() != 0 || f.cancels.Load() != 0 {
		t.Error("old events delivered")
	}
	f.view.Context(1, "opaque-context")
	f.request()
	if !slices.Equal(f.browser.ctx, []string{"opaque-context"}) || len(f.browser.opens) != 1 {
		t.Error("context / second request")
	}
	f.view.Dispose()

	missing := newFallback(func(*fallbackFixture) (ViewFactory, error) { return nil, errors.New("missing") })
	missing.view.Open(openFrame)
	if err := missing.request(); err == nil || !strings.Contains(err.Error(), "missing") || slices.Contains(missing.events, "old-disposed") {
		t.Errorf("missing browser: %v", err)
	}
	missing.oldCallbacks.Send(1, "live")
	if missing.sends.Load() != 1 {
		t.Error("embedded view must keep working")
	}

	gate := make(chan struct{})
	closing := newFallback(func(f *fallbackFixture) (ViewFactory, error) {
		<-gate
		return func(Callbacks) View { return f.browser }, nil
	})
	closing.view.Open(openFrame)
	done := make(chan error)
	go func() { done <- closing.request() }()
	time.Sleep(10 * time.Millisecond)
	closing.view.Close(1)
	close(gate)
	<-done
	if len(closing.browser.opens) != 0 {
		t.Error("closed attempt opened Chrome")
	}

	gate = make(chan struct{})
	disposed := newFallback(func(f *fallbackFixture) (ViewFactory, error) {
		<-gate
		return func(Callbacks) View { return f.browser }, nil
	})
	disposed.view.Open(openFrame)
	go func() { done <- disposed.request() }()
	time.Sleep(10 * time.Millisecond)
	disposed.view.Dispose()
	close(gate)
	<-done
	if len(disposed.browser.opens) != 0 {
		t.Error("disposed attempt resurrected")
	}

	waiting := newFallback(nil)
	waiting.embedded.gate = make(chan struct{})
	waiting.view.Open(openFrame)
	go func() { done <- waiting.request() }()
	time.Sleep(10 * time.Millisecond)
	contextDone := make(chan struct{})
	go func() { waiting.view.Context(1, "opaque-context"); close(contextDone) }()
	time.Sleep(10 * time.Millisecond)
	if len(waiting.browser.opens) != 0 {
		t.Error("Chrome opened before the embedded view closed")
	}
	close(waiting.embedded.gate)
	<-done
	<-contextDone
	if len(waiting.browser.ctx) != 1 {
		t.Error("context not delivered to the new view")
	}
}

func TestBrowserHelpers(t *testing.T) {
	native := BrowserLaunch(Browser{Kind: "native", Binary: "/usr/bin/google-chrome"}, "/tmp/profile", true)
	if native.StandardPipes || !slices.Contains(native.Args, "--remote-debugging-pipe") || !slices.Contains(native.Args, "--headless=new") || !slices.Contains(native.Args, "--incognito") {
		t.Errorf("native = %+v", native)
	}
	flatpak := BrowserLaunch(Browser{Kind: "flatpak", Binary: "/usr/bin/flatpak", Installation: "--user", Ref: "app/com.google.Chrome/x86_64/stable"}, "/tmp/profile", false)
	if !flatpak.StandardPipes || flatpak.Args[0] != "run" || !slices.Contains(flatpak.Args, "--die-with-parent") {
		t.Errorf("flatpak = %+v", flatpak)
	}
	frame := &Frame{URL: "https://login.live.com/x", Headers: map[string]string{"hostapp": "CloudExperienceHost"}}
	params := map[string]any{"frameId": "main", "resourceType": "Document", "request": map[string]any{"url": "https://login.live.com/x", "headers": map[string]any{"Accept": "*/*", "HostApp": "old"}}}
	headers := requestHeaders(params, "main", frame)
	if len(headers) != 2 || !slices.Contains(headers, headerEntry{"hostapp", "CloudExperienceHost"}) || slices.Contains(headers, headerEntry{"HostApp", "old"}) {
		t.Errorf("headers = %v", headers)
	}
	for _, change := range []func(map[string]any){func(p map[string]any) { p["frameId"] = "child" }, func(p map[string]any) { p["resourceType"] = "Script" },
		func(p map[string]any) { p["request"].(map[string]any)["url"] = "https://login.live.com/other" }} {
		copy := map[string]any{"frameId": "main", "resourceType": "Document", "request": map[string]any{"url": "https://login.live.com/x"}}
		change(copy)
		if requestHeaders(copy, "main", frame) != nil {
			t.Error("headers forwarded to a non-document request")
		}
	}
	context := &executionContext{Origin: "https://login.live.com"}
	context.AuxData.IsDefault, context.AuxData.FrameID = true, "main"
	if !bindingAllowed(context, "main", frame) || bindingAllowed(context, "other", frame) || bindingAllowed(context, "main", nil) {
		t.Error("binding check")
	}
	context.Origin = "https://evil.example"
	if bindingAllowed(context, "main", frame) {
		t.Error("foreign origin allowed")
	}
	if !strings.Contains(ContextMessage("ctx"), "CloudExperienceHost.getContext") {
		t.Error("context message")
	}
}
