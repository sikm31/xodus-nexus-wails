package shared

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func ptr(v float64) *float64 { return &v }

func TestParseBackendHelp(t *testing.T) {
	if !ParseBackendHelp("  login    Login\nOptions:\n  --external-webview-stdio  External UI\n").ExternalAuth {
		t.Error("external auth should be negotiated when advertised")
	}
	if ParseBackendHelp("  login    Login\n").ExternalAuth {
		t.Error("external auth must not be assumed")
	}
	parsed := ParseBackendHelp("Usage: xodus-cli <COMMAND>\n\nCommands:\n  account     Show stored account\n  download    Download files\n  login       \n  logout      \n  streaming   Stream\n  run         Run game\n  help        Help\n")
	if !parsed.Account || !parsed.Streaming || !parsed.Logout || !slices.Contains(parsed.Commands, "run") {
		t.Errorf("unexpected capabilities: %+v", parsed)
	}
}

func TestProgress(t *testing.T) {
	for line, want := range map[string]float64{
		"Downloading 42%": 42,
		"1.5 MiB / 3 MiB": 50,
		"Downloading                    512 MiB/2 GiB 12 MiB/s [###] 25%": 25,
	} {
		if got, ok := ProgressFromLine(line); !ok || got != want {
			t.Errorf("ProgressFromLine(%q) = %v, %v; want %v", line, got, ok, want)
		}
	}
	current, total, ok := ProgressBytesFromLine("Downloading                    512 MiB/2 GiB 12 MiB/s [###] 25%")
	if !ok || current != 512*(1<<20) || total != 2*(1<<30) {
		t.Errorf("byte counters = %v/%v", current, total)
	}
	if !strings.Contains(StripANSI("\x1b[32mok\x1b[0m\r"), "ok") {
		t.Error("ANSI not stripped")
	}
}

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		candidate string
		want      bool
	}{{"v0.5.25", true}, {"0.6.0", true}, {"1.0.0-rc.1", true}, {"v0.5.24", false}, {"0.5.23", false}, {"", false}, {"not-a-version", false}}
	for _, c := range cases {
		if got := IsNewerVersion(c.candidate, "0.5.24"); got != c.want {
			t.Errorf("IsNewerVersion(%q) = %v", c.candidate, got)
		}
	}
}

func TestEstimateTimeRemaining(t *testing.T) {
	cases := []struct {
		rate              string
		downloaded, total float64
		want              string
	}{
		{"86.4 MiB/s", 18_612_245_504, 44_258_432_000, "~5m left"},
		{"3 KiB/s", 0, 921_600, "~5m left"},
		{"1 KB/s", 1000, 481_000, "~8m left"},
		{"2 MiB/s", 10 * (1 << 20), 50 * (1 << 20), "~20s left"},
		{"512 KiB/s", 0, 4 * (1 << 30), "~2h 17m left"},
		{"0 MiB/s", 100, 200, ""},
		{"12 parsecs/s", 100, 200, ""},
		{"12 MiB/s", 200, 200, ""},
	}
	for _, c := range cases {
		if got := EstimateTimeRemaining(c.rate, ptr(c.downloaded), ptr(c.total)); got != c.want {
			t.Errorf("EstimateTimeRemaining(%q) = %q, want %q", c.rate, got, c.want)
		}
	}
	if EstimateTimeRemaining("", ptr(100), ptr(200)) != "" || EstimateTimeRemaining("12 MiB/s", nil, ptr(200)) != "" ||
		EstimateTimeRemaining("12 MiB/s", ptr(200), nil) != "" {
		t.Error("missing inputs must not produce an estimate")
	}
}

func TestDownloadFailure(t *testing.T) {
	marker := "NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-http 403"
	text := DownloadFailure([]string{marker, "untrusted-provider-message"}, 1)
	if !strings.Contains(text, "purchased edition") || strings.Contains(text, "untrusted-provider-message") {
		t.Errorf("license failure: %q", text)
	}
	limit := DownloadFailure([]string{"NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-device-limit 200", "untrusted-provider-message"}, 1)
	if !strings.Contains(limit, "Microsoft Store device limit reached") || !strings.Contains(limit, "https://account.microsoft.com/devices/content") || strings.Contains(limit, "untrusted") {
		t.Errorf("device limit: %q", limit)
	}
	if !strings.Contains(DownloadFailure([]string{marker, "NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-parsed 200", "extraction failed"}, 1), "extraction failed") {
		t.Error("parsed license must clear the earlier failure")
	}
	panicLog := []string{
		"0% · Initializing 0 B/9.13 GiB",
		"thread 'main' panicked at streaming.rs:280:14:",
		`ok: Header(Io(Custom { kind: UnexpectedEof, error: "cache ended before cached_len" }))`,
		"note: run with `RUST_BACKTRACE=1` environment variable to display a backtrace",
	}
	if !strings.Contains(DownloadFailure(panicLog, 101), "cache ended before cached_len") {
		t.Error("panic cause should be shown instead of the backtrace hint")
	}
}

func TestSafeProductID(t *testing.T) {
	if got, err := SafeProductID(" 9n4pt8hgcdhq "); err != nil || got != "9N4PT8HGCDHQ" {
		t.Errorf("SafeProductID = %q, %v", got, err)
	}
	if _, err := SafeProductID("foo; rm -rf"); err == nil {
		t.Error("shell-like input must be rejected")
	}
}

func contentRecord(stage string, status int) string {
	return fmt.Sprintf("NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 %s %d", stage, status)
}

func TestContentLicense(t *testing.T) {
	text := ContentLicenseFailure([]string{contentRecord("content-device-limit", 200)})
	if !strings.Contains(text, "unlink") || regexp.MustCompile(`purchased edition|Sign in again`).MatchString(text) {
		t.Errorf("device limit: %q", text)
	}
	if !strings.Contains(ContentLicenseFailure([]string{contentRecord("content-http", 501)}), "server error") {
		t.Error("HTTP 501 is a server error")
	}
	if ContentLicenseFailure([]string{contentRecord("content-device-limit", 200), contentRecord("content-parsed", 200)}) != "" {
		t.Error("parsed response clears the failure")
	}
	for status, message := range map[int]string{401: "Sign in again", 403: "purchased edition", 429: "rate-limiting", 503: "server error"} {
		if !strings.Contains(ContentLicenseFailure([]string{contentRecord("content-http", status)}), message) {
			t.Errorf("HTTP %d should mention %q", status, message)
		}
	}
	if !strings.Contains(ContentLicenseFailure([]string{contentRecord("content-timeout", 0)}), "timed out") ||
		!strings.Contains(ContentLicenseFailure([]string{contentRecord("content-schema", 200)}), "unsupported") {
		t.Error("transport/response failures")
	}
	for _, raw := range []string{"prefix " + contentRecord("content-http", 403), contentRecord("content-http", 403) + " secret",
		contentRecord("unknown", 200), contentRecord("content-http", 999), contentRecord("content-http", -1), "private@example.test t=SECRET",
		contentRecord("content-schema", 200) + "\n", contentRecord("content-schema", 200) + "\r"} {
		if _, ok := ContentLicenseDiagnostic(raw); ok {
			t.Errorf("accepted %q", raw)
		}
	}
	for stage := range ContentLicenseStages {
		if got, ok := ContentLicenseDiagnostic(contentRecord(stage, 200)); !ok || got != (Diagnostic{stage, 200}) {
			t.Errorf("stage %s rejected", stage)
		}
		if regexp.MustCompile(`example.test|SECRET`).MatchString(ContentLicenseFailure([]string{"private@example.test t=SECRET", contentRecord(stage, 200)})) {
			t.Errorf("stage %s leaked provider text", stage)
		}
	}
	old := append([]string{contentRecord("content-http", 503)}, slices.Repeat([]string{"other"}, 256)...)
	if ContentLicenseFailure(old) != "" {
		t.Error("records older than 256 lines must be ignored")
	}
}

func transferRecord(stage string, status int) string {
	return fmt.Sprintf("NEXUS_TRANSFER_DIAGNOSTIC_V1 %s %d", stage, status)
}

func TestTransferFailure(t *testing.T) {
	for stage, text := range map[string]string{"transfer-headers-timeout": "did not respond", "transfer-idle-timeout": "stopped receiving",
		"transfer-request": "connection failed", "transfer-read": "connection failed", "transfer-eof": "connection failed",
		"transfer-range": "Unsafe data", "transfer-encoding": "Unsafe data", "transfer-size": "Unsafe data", "transfer-io": "free space",
		"metadata-invalid": "metadata could not be read"} {
		if !strings.Contains(TransferFailure([]string{transferRecord(stage, 0)}), text) {
			t.Errorf("%s should mention %q", stage, text)
		}
	}
	for _, status := range []int{401, 403, 429, 503} {
		text := TransferFailure([]string{transferRecord("transfer-http", status)})
		if !strings.Contains(text, fmt.Sprintf("HTTP %d", status)) || !strings.Contains(text, "Partial files are kept") ||
			regexp.MustCompile(`do not own|license|Sign in`).MatchString(text) {
			t.Errorf("HTTP %d: %q", status, text)
		}
	}
	for _, raw := range []string{transferRecord("unknown", 0), transferRecord("transfer-http", 999), transferRecord("transfer-http", -1),
		transferRecord("transfer-http", 99), transferRecord("transfer-http", 600), transferRecord("transfer-http", 0) + " SECRET",
		"prefix " + transferRecord("transfer-http", 0), transferRecord("transfer-http", 0) + "\n", "https://signed.example/?token=SECRET"} {
		if _, ok := TransferDiagnostic(raw); ok {
			t.Errorf("accepted %q", raw)
		}
	}
	if !strings.Contains(TransferFailure([]string{transferRecord("transfer-http", 403), transferRecord("transfer-cancelled", 0)}), "HTTP 403") ||
		TransferFailure([]string{transferRecord("transfer-cancelled", 0)}) != "" {
		t.Error("cancelled siblings must not hide the real cause")
	}
	if !strings.Contains(DownloadFailure([]string{"NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-parsed 200", transferRecord("transfer-http", 503), "raw secret signed url"}, 1), "HTTP 503") ||
		strings.Contains(DownloadFailure([]string{transferRecord("transfer-http", 503), "raw secret signed url"}, 1), "secret") {
		t.Error("transfer failure must win over raw output")
	}
}
