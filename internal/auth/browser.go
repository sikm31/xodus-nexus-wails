package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"syscall"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
)

type Browser struct {
	Kind         string // native | flatpak
	Binary       string
	Installation string // --user | --system
	Ref          string
}

var ErrBrowserNotFound = errors.New("Google Chrome was not found. Install Google Chrome (native or Flatpak from Discover), then try again.")

func executableFile(file string) bool {
	info, err := os.Stat(file)
	return err == nil && info.Mode().IsRegular() && syscall.Access(file, 1) == nil
}

var flatpakRef = regexp.MustCompile(`^app/com\.google\.Chrome/(?:x86_64|aarch64)/stable$`)

// FindBrowser prefers native Chrome. A setting or desktop Exec line is never
// interpreted as a command.
func FindBrowser(env launcherenv.Env) (Browser, error) {
	candidates := []string{"/usr/bin/google-chrome-stable", "/usr/bin/google-chrome", "/opt/google/chrome/google-chrome"}
	var path []string
	for _, directory := range filepath.SplitList(env["PATH"]) {
		if filepath.IsAbs(directory) {
			path = append(path, directory)
			candidates = append(candidates, filepath.Join(directory, "google-chrome-stable"), filepath.Join(directory, "google-chrome"))
		}
	}
	var seen []string
	for _, file := range candidates {
		if slices.Contains(seen, file) {
			continue
		}
		seen = append(seen, file)
		if executableFile(file) {
			return Browser{Kind: "native", Binary: file}, nil
		}
	}
	flatpaks := []string{"/usr/bin/flatpak", "/bin/flatpak"}
	for _, directory := range path {
		flatpaks = append(flatpaks, filepath.Join(directory, "flatpak"))
	}
	for _, binary := range flatpaks {
		if !executableFile(binary) {
			continue
		}
		for _, installation := range []string{"--user", "--system"} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			cmd := exec.CommandContext(ctx, binary, "info", installation, "--show-ref", "com.google.Chrome")
			cmd.Env = launcherenv.External(env).List()
			out, err := cmd.Output()
			cancel()
			if ref := string(trimSpace(out)); err == nil && len(out) <= 4096 && flatpakRef.MatchString(ref) {
				return Browser{Kind: "flatpak", Binary: binary, Installation: installation, Ref: ref}, nil
			}
		}
		break
	}
	return Browser{}, ErrBrowserNotFound
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

type Launch struct {
	Command string
	Args    []string
	// Pipes are fd 3/4 for native Chrome and stdin/stdout through Flatpak.
	StandardPipes bool
}

// BrowserLaunch builds the disposable Chrome command line. Flatpak preserves
// standard pipes; a constant inner command maps them to CDP, since its run
// command does not support flatpak-spawn's --forward-fd option.
func BrowserLaunch(browser Browser, profile string, headless bool) Launch {
	args := []string{"--user-data-dir=" + filepath.Join(profile, "profile"), "--incognito", "--no-first-run", "--no-default-browser-check",
		"--disable-sync", "--disable-extensions", "--remote-debugging-pipe"}
	if headless {
		args = append(args, "--headless=new")
	}
	args = append(args, "about:blank")
	if browser.Kind == "native" {
		return Launch{Command: browser.Binary, Args: args}
	}
	return Launch{Command: browser.Binary, StandardPipes: true, Args: append([]string{"run", browser.Installation, "--die-with-parent", "--filesystem=" + profile,
		"--env=XDG_CONFIG_HOME=" + filepath.Join(profile, "config"), "--env=XDG_CACHE_HOME=" + filepath.Join(profile, "cache"),
		"--env=XDG_DATA_HOME=" + filepath.Join(profile, "data"), "--env=TMPDIR=" + filepath.Join(profile, "tmp"),
		"--command=sh", browser.Ref, "-c", `exec /app/bin/chrome "$@" 3<&0 4>&1 </dev/null >/dev/null`, "nexus-browser-auth"}, args...)}
}

// bridgeScript runs in the main world of trusted Microsoft pages only. It
// never changes WebAuthn.
func bridgeScript(name string) string {
	hosts, _ := json.Marshal(Hosts)
	quoted, _ := json.Marshal(name)
	return `((name, hosts) => {
  if (window !== window.top || location.protocol !== 'https:' || location.port || !hosts.includes(location.hostname)) return;
  const channel = globalThis[name];
  const notify = payload => { if (typeof payload === 'string' && payload.length <= 65536) channel(payload); };
  Object.defineProperty(window.external, 'notify', { value: notify, configurable: false, writable: false });
  window.addEventListener('DOMContentLoaded', () => {
    if (location.hostname === 'login.live.com' && location.pathname === '/ppsecure/post.srf') {
      const data = window.ServerData;
      if (data && typeof data.sDAToken === 'string' && data.sDAToken) notify(JSON.stringify(data));
    }
  }, { once: true });
})(` + string(quoted) + `,` + string(hosts) + `)`
}

type executionContext struct {
	ID      float64 `json:"id"`
	Origin  string  `json:"origin"`
	AuxData struct {
		IsDefault bool   `json:"isDefault"`
		FrameID   string `json:"frameId"`
	} `json:"auxData"`
}

func bindingAllowed(context *executionContext, mainFrame string, frame *Frame) bool {
	return frame != nil && mainFrame != "" && context != nil && context.AuxData.IsDefault && context.AuxData.FrameID == mainFrame && TrustedURL(context.Origin)
}

type headerEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// requestHeaders adds the backend's headers to the main-frame document request
// of the active sign-in URL only, never to a redirect or iframe.
func requestHeaders(params map[string]any, mainFrame string, frame *Frame) []headerEntry {
	request, _ := params["request"].(map[string]any)
	requestURL, _ := request["url"].(string)
	if frame == nil || mainFrame == "" || params["frameId"] != mainFrame || params["resourceType"] != "Document" || requestURL != frame.URL || !TrustedURL(frame.URL) {
		return nil
	}
	var out []headerEntry
	original, _ := request["headers"].(map[string]any)
	for name, value := range original {
		if _, replaced := frame.Headers[lower(name)]; replaced {
			continue
		}
		if text, ok := value.(string); ok {
			out = append(out, headerEntry{name, text})
		}
	}
	for name, value := range frame.Headers {
		out = append(out, headerEntry{name, value})
	}
	return out
}

func lower(value string) string {
	out := []byte(value)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return string(out)
}

// BrowserViewFactory hosts sign-in in a dedicated disposable Chrome,
// controlled through inherited pipes, never default-profile debugging.
func BrowserViewFactory(browser Browser, env launcherenv.Env, headless bool) ViewFactory {
	return func(callbacks Callbacks) View {
		token := make([]byte, 24)
		rand.Read(token)
		return &browserView{browser: browser, env: env, headless: headless, callbacks: callbacks, binding: "nexusAuth" + hex.EncodeToString(token),
			contexts: map[float64]*executionContext{}}
	}
}

type browserView struct {
	browser   Browser
	env       launcherenv.Env
	headless  bool
	callbacks Callbacks
	binding   string

	mu                             sync.Mutex
	cmd                            *exec.Cmd
	cdp                            *CDP
	profile                        string
	sessionID, targetID, mainFrame string
	frame                          *Frame
	contexts                       map[float64]*executionContext
	disposed, failed, initialized  bool
	initErr                        error
	exited                         chan struct{}
}

func (v *browserView) failure() {
	v.mu.Lock()
	if v.disposed || v.failed {
		v.mu.Unlock()
		return
	}
	v.failed = true
	v.mu.Unlock()
	v.callbacks.Fail()
}

func (v *browserView) call(method string, params any, out any) error {
	v.mu.Lock()
	cdp, session, disposed := v.cdp, v.sessionID, v.disposed
	v.mu.Unlock()
	if cdp == nil || disposed {
		return errors.New("Browser sign-in is closed.")
	}
	return cdp.Call(method, params, session, out)
}

func (v *browserView) evaluate(expression string, contextID float64) (any, error) {
	var result struct {
		Result           struct{ Value any } `json:"result"`
		ExceptionDetails json.RawMessage     `json:"exceptionDetails"`
	}
	if err := v.call("Runtime.evaluate", map[string]any{"expression": expression, "contextId": contextID, "returnByValue": true}, &result); err != nil {
		return nil, err
	}
	if len(result.ExceptionDetails) > 0 && string(result.ExceptionDetails) != "null" {
		return nil, errors.New("Browser sign-in page failed.")
	}
	return result.Result.Value, nil
}

func (v *browserView) handle(event CDPEvent) {
	p := event.Params
	v.mu.Lock()
	if event.Method == "Target.targetDestroyed" && p["targetId"] == v.targetID {
		frame := v.frame
		v.mu.Unlock()
		if frame != nil {
			v.callbacks.Cancel(frame.Session)
		} else {
			v.failure()
		}
		return
	}
	if event.SessionID != v.sessionID || v.disposed {
		v.mu.Unlock()
		return
	}
	switch event.Method {
	case "Runtime.executionContextCreated":
		raw, _ := json.Marshal(p["context"])
		var context executionContext
		if json.Unmarshal(raw, &context) == nil {
			v.contexts[context.ID] = &context
		}
		v.mu.Unlock()
		return
	case "Runtime.executionContextDestroyed":
		id, _ := p["executionContextId"].(float64)
		delete(v.contexts, id)
		v.mu.Unlock()
		return
	case "Runtime.executionContextsCleared":
		clear(v.contexts)
		v.mu.Unlock()
		return
	case "Page.frameNavigated":
		frame, _ := p["frame"].(map[string]any)
		if _, child := frame["parentId"]; !child {
			v.mainFrame, _ = frame["id"].(string)
			target, _ := frame["url"].(string)
			active := v.frame
			v.mu.Unlock()
			if active != nil && !TrustedURL(target) && target != "about:blank" {
				v.failure()
			}
			return
		}
		v.mu.Unlock()
		return
	case "Page.windowOpen":
		v.mu.Unlock()
		v.failure()
		return
	case "Fetch.requestPaused":
		// Intercept only Microsoft login requests. Never forward host headers to a redirect or iframe.
		headers := requestHeaders(p, v.mainFrame, v.frame)
		v.mu.Unlock()
		params := map[string]any{"requestId": p["requestId"]}
		if headers != nil {
			params["headers"] = headers
		}
		if err := v.call("Fetch.continueRequest", params, nil); err != nil {
			v.failure()
		}
		return
	}
	payload, isString := p["payload"].(string)
	if event.Method != "Runtime.bindingCalled" || p["name"] != v.binding || !isString || len(payload) > 65_536 {
		v.mu.Unlock()
		return
	}
	contextID, _ := p["executionContextId"].(float64)
	context := v.contexts[contextID]
	if !bindingAllowed(context, v.mainFrame, v.frame) {
		v.mu.Unlock()
		return
	}
	active := v.frame
	v.mu.Unlock()
	// Recheck this realm after the async command, not a new page's default realm.
	origin, err := v.evaluate("location.origin", contextID)
	text, _ := origin.(string)
	v.mu.Lock()
	current := v.frame
	v.mu.Unlock()
	if err != nil || current != active || !TrustedURL(text) || text != context.Origin {
		return
	}
	v.callbacks.Send(active.Session, payload)
}

func (v *browserView) initialize() error {
	profile, err := os.MkdirTemp("", "nexus-browser-auth-")
	if err != nil {
		return err
	}
	os.Chmod(profile, 0o700)
	v.mu.Lock()
	v.profile = profile
	disposed := v.disposed
	v.mu.Unlock()
	if disposed {
		return errors.New("Browser sign-in is closed.")
	}
	for _, name := range []string{"profile", "config", "cache", "data", "tmp"} {
		if err := os.Mkdir(filepath.Join(profile, name), 0o700); err != nil {
			return err
		}
	}
	launch := BrowserLaunch(v.browser, profile, v.headless)
	cmd := exec.Command(launch.Command, launch.Args...)
	env := launcherenv.External(v.env)
	env["TMPDIR"] = filepath.Join(profile, "tmp")
	cmd.Env = env.List()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var input io.WriteCloser
	var output io.ReadCloser
	if launch.StandardPipes {
		if input, err = cmd.StdinPipe(); err != nil {
			return err
		}
		if output, err = cmd.StdoutPipe(); err != nil {
			return err
		}
	} else {
		toChrome, chromeInput, err := os.Pipe()
		if err != nil {
			return err
		}
		chromeOutput, fromChrome, err := os.Pipe()
		if err != nil {
			return err
		}
		cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
		defer toChrome.Close()
		defer fromChrome.Close()
		input, output = chromeInput, chromeOutput
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	v.mu.Lock()
	v.cmd, v.exited = cmd, exited
	v.mu.Unlock()
	go func() {
		cmd.Wait()
		close(exited)
		v.mu.Lock()
		disposed, frame := v.disposed, v.frame
		v.mu.Unlock()
		if !disposed {
			if frame != nil {
				v.callbacks.Cancel(frame.Session)
			} else {
				v.failure()
			}
		}
	}()
	cdp := NewCDP(input, output, func(event CDPEvent) { go v.handle(event) }, v.failure, 0)
	v.mu.Lock()
	v.cdp = cdp
	v.mu.Unlock()
	var targets struct {
		TargetInfos []struct {
			Type, URL, TargetID string
		} `json:"targetInfos"`
	}
	if err := cdp.Call("Target.getTargets", nil, "", &targets); err != nil {
		return err
	}
	index := slices.IndexFunc(targets.TargetInfos, func(t struct{ Type, URL, TargetID string }) bool { return t.Type == "page" && t.URL == "about:blank" })
	if index < 0 {
		return errors.New("Browser sign-in window is unavailable.")
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := cdp.Call("Target.attachToTarget", map[string]any{"targetId": targets.TargetInfos[index].TargetID, "flatten": true}, "", &attached); err != nil {
		return err
	}
	v.mu.Lock()
	v.targetID, v.sessionID = targets.TargetInfos[index].TargetID, attached.SessionID
	v.mu.Unlock()
	if err := cdp.Call("Target.setDiscoverTargets", map[string]any{"discover": true}, "", nil); err != nil {
		return err
	}
	if err := v.call("Page.enable", nil, nil); err != nil {
		return err
	}
	var tree struct {
		FrameTree struct {
			Frame struct{ ID string } `json:"frame"`
		} `json:"frameTree"`
	}
	if err := v.call("Page.getFrameTree", nil, &tree); err != nil {
		return err
	}
	v.mu.Lock()
	v.mainFrame = tree.FrameTree.Frame.ID
	v.mu.Unlock()
	for _, step := range []struct {
		method string
		params any
	}{
		{"Runtime.enable", nil},
		{"Runtime.addBinding", map[string]any{"name": v.binding}},
		{"Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": bridgeScript(v.binding)}},
		{"Browser.setDownloadBehavior", map[string]any{"behavior": "deny"}},
		{"Fetch.enable", map[string]any{"patterns": []any{map[string]any{"urlPattern": "https://login.live.com/*", "requestStage": "Request"}}}},
	} {
		if err := v.call(step.method, step.params, nil); err != nil {
			return err
		}
	}
	return nil
}

func (v *browserView) Open(frame Frame) error {
	v.mu.Lock()
	if v.disposed || !TrustedURL(frame.URL) {
		v.mu.Unlock()
		return errors.New("Unsupported browser sign-in URL.")
	}
	if !v.initialized {
		v.initialized = true
		v.mu.Unlock()
		v.initErr = v.initialize()
		v.mu.Lock()
	}
	if v.initErr != nil {
		v.mu.Unlock()
		return v.initErr
	}
	if v.disposed {
		v.mu.Unlock()
		return errors.New("Browser sign-in is closed.")
	}
	v.frame = &frame
	v.mu.Unlock()
	var result struct {
		ErrorText string `json:"errorText"`
	}
	if err := v.call("Page.navigate", map[string]any{"url": frame.URL}, &result); err != nil {
		return err
	}
	if result.ErrorText != "" {
		return errors.New("Browser sign-in page did not load.")
	}
	return nil
}

func (v *browserView) Close(session int64) {
	v.mu.Lock()
	if v.frame != nil && v.frame.Session == session {
		v.frame = nil
	}
	v.mu.Unlock()
}

func (v *browserView) Context(session int64, context string) error {
	v.mu.Lock()
	if v.disposed || v.frame == nil || v.frame.Session != session {
		v.mu.Unlock()
		return nil
	}
	active := v.frame
	var realm *executionContext
	for _, candidate := range v.contexts {
		if bindingAllowed(candidate, v.mainFrame, active) {
			realm = candidate
			break
		}
	}
	v.mu.Unlock()
	if realm == nil {
		return nil
	}
	origin, err := v.evaluate("location.origin", realm.ID)
	if err != nil {
		return err
	}
	text, _ := origin.(string)
	v.mu.Lock()
	stale := v.frame != active || v.contexts[realm.ID] != realm
	v.mu.Unlock()
	if stale || !TrustedURL(text) || text != realm.Origin {
		return nil
	}
	_, err = v.evaluate(ContextMessage(context), realm.ID)
	return err
}

func (v *browserView) Dispose() {
	v.mu.Lock()
	if v.disposed {
		v.mu.Unlock()
		return
	}
	v.disposed, v.frame = true, nil
	cdp, cmd, exited, profile := v.cdp, v.cmd, v.exited, v.profile
	v.mu.Unlock()
	if cdp != nil {
		done := make(chan struct{})
		go func() { cdp.Call("Browser.close", nil, "", nil); close(done) }()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
		cdp.Close()
	}
	if cmd != nil && cmd.Process != nil {
		select {
		case <-exited:
		default:
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(time.Second):
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				select {
				case <-exited:
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}
	// The mkdtemp-owned path only, never a browser profile supplied through
	// settings or environment.
	if profile != "" {
		if exited == nil {
			os.RemoveAll(profile)
		} else {
			select {
			case <-exited:
				os.RemoveAll(profile)
			default:
			}
		}
	}
}
