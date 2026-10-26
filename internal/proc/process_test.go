package proc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"xodus-nexus-wails/internal/diagnostics"
	"xodus-nexus-wails/internal/launcherenv"
)

type lineLog struct {
	sync.Mutex
	lines []string
}

func (l *lineLog) add(line string) { l.Lock(); l.lines = append(l.lines, line); l.Unlock() }

func run(t *testing.T, script string, options RunOptions) Result {
	t.Helper()
	result, err := Run("sh", []string{"-c", script}, options)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStructuredStdoutWithInterleavedDiagnostics(t *testing.T) {
	var lines lineLog
	script := `printf '{"value":"'; sleep 0.02; printf 'diagnostic\n%s\n' "$(head -c 5000 /dev/zero | tr '\0' x)" >&2; sleep 0.02; printf 'ok"}\n'`
	result := run(t, script, RunOptions{StdoutOnly: true, MaxOutput: 100, OnLine: lines.add})
	var value map[string]string
	if result.Code != 0 || json.Unmarshal([]byte(result.Output), &value) != nil || value["value"] != "ok" {
		t.Fatalf("result = %+v", result)
	}
	if !slices.Contains(lines.lines, "diagnostic") || !slices.Contains(lines.lines, `{"value":"ok"}`) {
		t.Errorf("lines = %q", lines.lines)
	}
}

func TestSplitUTF8AndANSI(t *testing.T) {
	var lines lineLog
	script := `printf '\342'; printf 'stderr\n' >&2; sleep 0.01; printf '\202\254\033['; sleep 0.01; printf '32m Downloading 1 MiB/2 MiB 50%%\r'`
	result := run(t, script, RunOptions{OnLine: lines.add})
	if result.Code != 0 || !slices.Contains(lines.lines, "stderr") || !slices.Contains(lines.lines, "€ Downloading 1 MiB/2 MiB 50%") {
		t.Errorf("code %d lines %q", result.Code, lines.lines)
	}
}

func TestCarriageReturnUpdatesAndNoShell(t *testing.T) {
	var lines lineLog
	result, err := Run("printf", []string{`Downloading 1 MiB/10 MiB 10%%\rDownloading 5 MiB/10 MiB 50%%\n`}, RunOptions{OnLine: lines.add})
	if err != nil || result.Code != 0 {
		t.Fatal(err, result)
	}
	joined := strings.Join(lines.lines, "|")
	if !strings.Contains(joined, "1 MiB/10 MiB") || !strings.Contains(joined, "5 MiB/10 MiB") {
		t.Errorf("lines = %q", lines.lines)
	}
	result, _ = Run("printf", []string{`%s\n`, "a'b; echo UNSAFE"}, RunOptions{})
	if !strings.Contains(result.Output, "a'b; echo UNSAFE") {
		t.Errorf("output = %q", result.Output)
	}
}

func TestStdinControls(t *testing.T) {
	var lines lineLog
	tracked, err := Spawn("sh", []string{"-c", `while read -r line; do echo "got:$line"; [ "$line" = resume ] && exit 0; done`},
		RunOptions{OnLine: lines.add})
	if err != nil {
		t.Fatal(err)
	}
	tracked.Stdin.Write([]byte("pause\nresume\n"))
	if result := tracked.Wait(); result.Code != 0 || !slices.Equal(lines.lines, []string{"got:pause", "got:resume"}) {
		t.Errorf("result %+v lines %q", result, lines.lines)
	}
}

func TestMissingCommandUsesChildPath(t *testing.T) {
	env := launcherenv.FromOS()
	env["PATH"] = ""
	if _, err := Run("sh", nil, RunOptions{Env: env}); err == nil {
		t.Error("lookup must use the child's PATH")
	}
}

func TestExternalEnvironmentForChildren(t *testing.T) {
	env := launcherenv.FromOS()
	for k, v := range map[string]string{"XDG_CURRENT_DESKTOP": "gamescope", "GTK_IM_MODULE": "simple", "XODUS_NEXUS_STEAM_IME": "1",
		"QT_IM_MODULE": "steam", "SteamGameId": "123", "DISPLAY": ":1"} {
		env[k] = v
	}
	result := run(t, `printf '%s|%s|%s|%s|%s' "$GTK_IM_MODULE" "${XODUS_NEXUS_STEAM_IME-unset}" "$QT_IM_MODULE" "$SteamGameId" "$DISPLAY"`, RunOptions{Env: env})
	if result.Output != "Steam|unset|steam|123|:1" {
		t.Errorf("child environment = %q", result.Output)
	}
}

func TestOutputDrainAfterExit(t *testing.T) {
	start := time.Now()
	result := run(t, `sleep 5 & echo ready; exit 3`, RunOptions{DrainAfterExit: true, OutputDrain: 100 * time.Millisecond})
	if result.Code != 3 || !strings.Contains(result.Output, "ready") || time.Since(start) > 3*time.Second {
		t.Errorf("result %+v after %v", result, time.Since(start))
	}
}

func TestKillReportsSignal(t *testing.T) {
	tracked, err := Spawn("sleep", []string{"10"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tracked.Kill()
	if result := tracked.Wait(); result.Code != -1 || !tracked.Exited() {
		t.Errorf("result = %+v", result)
	}
	if err := tracked.Kill(); err != nil {
		t.Errorf("killing an exited child: %v", err)
	}
}

func TestObservesCLIWithoutCapturingPrivateData(t *testing.T) {
	root := t.TempDir()
	log := diagnostics.NewBackend(root, nil, 0)
	diagnostics.SetBackend(log)
	defer diagnostics.SetBackend(nil)
	binary := filepath.Join(root, "xodus-cli")
	os.WriteFile(binary, []byte("#!/bin/sh\nprintf '{\"account\":\"user@example.test\",\"secret\":\"synthetic-token\"}'\nprintf 'raw-secret\\nNEXUS_LIBRARY_DIAGNOSTIC_V1 entitlements-http 401 0\\n' >&2\nexit 1\n"), 0o755)
	result, err := Run(binary, []string{"library", "--json", "--market", "US"}, RunOptions{})
	if err != nil || result.Code != 1 {
		t.Fatal(err, result)
	}
	content, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content.Content, "entitlements-http") || !strings.Contains(content.Content, `"status":401`) || !strings.Contains(content.Content, `"code":1`) ||
		regexp.MustCompile(`example.test|synthetic-token|raw-secret|--json|`+regexp.QuoteMeta(filepath.Base(root))).MatchString(content.Content) {
		t.Errorf("log = %s", content.Content)
	}
	// A logger failure must not interfere with the process.
	os.RemoveAll(filepath.Join(root, "backend-diagnostics"))
	os.WriteFile(filepath.Join(root, "backend-diagnostics"), []byte("occupied"), 0o600)
	if result, err := Run(binary, []string{"achievements", "12345678", "--json"}, RunOptions{}); err != nil || result.Code != 1 {
		t.Error(err, result)
	}
	log.Flush()
}
