package diagnostics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func sessionRecord(stage string) string { return "NEXUS_SESSION_DIAGNOSTIC_V1 " + stage + "\n" }

func TestSessionFailureMessage(t *testing.T) {
	frame := "NEXUS_TICKET_DIAGNOSTIC_V1 ticket-interaction-required 800488AB"
	if got, ok := TicketFailureDiagnostic(frame); !ok || got != (TicketFailure{"ticket-interaction-required", "800488AB"}) {
		t.Errorf("ticket frame = %+v", got)
	}
	msg := SessionFailureMessage(frame)
	if !strings.Contains(msg, "interactive sign-in (code 0x800488AB)") || !strings.Contains(msg, "has not been deleted") {
		t.Errorf("interaction message = %q", msg)
	}
	for input, want := range map[string]string{
		"NEXUS_TICKET_DIAGNOSTIC_V1 ticket-refused 800488AB":                                              "Microsoft refused",
		sessionRecord("ticket-request-build"):                                                             "build a ticket request",
		sessionRecord("ticket-response-parse"):                                                            "unreadable response",
		sessionRecord("ticket-response-signature"):                                                        "verification failed",
		sessionRecord("session-user-expired"):                                                             "Sign in again",
		sessionRecord("ticket-timeout"):                                                                   "timed out",
		sessionRecord("ticket-connect"):                                                                   "connection",
		sessionRecord("session-device-refresh-failed"):                                                    "could not be renewed",
		sessionRecord("session-account-changed"):                                                          "account changed",
		sessionRecord("ticket-timeout") + sessionRecord("ticket-retry") + sessionRecord("ticket-connect"): "Could not connect",
	} {
		if got := SessionFailureMessage(input); !strings.Contains(got, want) {
			t.Errorf("SessionFailureMessage(%q) = %q, want %q", input, got, want)
		}
	}
	if SessionFailureMessage(frame+"\n"+sessionRecord("ticket-response-received")) != "" {
		t.Error("received response clears the failure")
	}
	if strings.Contains(SessionFailureMessage(frame+"\n"+sessionRecord("ticket-timeout")), "800488AB") {
		t.Error("service code must not leak into a later stage")
	}
	for _, raw := range []string{frame + " secret", "prefix " + frame, strings.Replace(frame, "800488AB", "800488AZ", 1),
		strings.Replace(frame, "800488AB", "800488ab", 1), strings.Replace(frame, "800488AB", "secret@example.test", 1),
		strings.Replace(frame, "800488AB", "800488A", 1), strings.Replace(frame, "800488AB", "800488ABC", 1),
		"private@example.test t=SECRET&p=SECRET", sessionRecord("secret"), "prefix " + sessionRecord("session-user-expired"),
		strings.TrimRight(sessionRecord("ticket-timeout"), "\n") + " SECRET\n", "NEXUS_SESSION_DIAGNOSTIC_V1",
		sessionRecord("ticket-retry"), sessionRecord("session-device-refresh"), "NEXUS_SESSION_DIAGNOSTIC_V1 " + strings.Repeat("x", 1000),
		sessionRecord("ticket-timeout") + sessionRecord("ticket-retry") + sessionRecord("ticket-response-received") + "entitlements rejected",
		sessionRecord("session-user-expired") + strings.Repeat("x", 70_000)} {
		if got := SessionFailureMessage(raw); got != "" {
			t.Errorf("SessionFailureMessage(%.80q) = %q", raw, got)
		}
	}
	for code := range SessionStages {
		if regexp.MustCompile(`example\.test|raw-secret`).MatchString(SessionFailureMessage("user@example.test\n" + sessionRecord(code) + "raw-secret")) {
			t.Errorf("stage %s leaked output", code)
		}
	}
}

func TestCredentialErrorReader(t *testing.T) {
	for _, code := range []string{"keyring-unavailable", "keyring-invalid-data", "keyring-write-verification", "keyring-missing-data", "device-authentication", "credential-io"} {
		bytes := []byte("private-account-data\nNEXUS_CREDENTIAL_ERROR_V1 " + code + "\r\n")
		for split := 0; split <= len(bytes); split++ {
			reader := NewCredentialErrorReader()
			reader.Push(bytes[:split])
			reader.Push(bytes[split:])
			if reader.Message == "" || strings.Contains(reader.Message, code) || strings.Contains(reader.Message, "private-account-data") {
				t.Fatalf("code %s split %d: %q", code, split, reader.Message)
			}
		}
	}
	for _, value := range []string{"private-secret\n", "NEXUS_CREDENTIAL_ERROR_V1 private-secret\n", "prefix NEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data\n",
		"NEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data private-secret\n", "NEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data",
		strings.Repeat("x", 1000) + "NEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data\n", "éNEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data\n"} {
		reader := NewCredentialErrorReader()
		reader.Push([]byte(value))
		if reader.Message != "" {
			t.Errorf("accepted %.60q", value)
		}
	}
	reader := NewCredentialErrorReader()
	reader.Push([]byte(strings.Repeat("x", 10_000) + "\nNEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data\n"))
	if !strings.Contains(reader.Message, "empty or invalid") {
		t.Error("reader must recover after an oversized line")
	}
	limited := NewCredentialErrorReader()
	limited.Push([]byte(strings.Repeat("x", 2*1024*1024)))
	limited.Push([]byte("\nNEXUS_CREDENTIAL_ERROR_V1 keyring-invalid-data\n"))
	if limited.Message != "" {
		t.Error("total parsing must be bounded")
	}
}

func TestDiagnosticTailAndCopy(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	os.Mkdir(logs, 0o755)
	file := filepath.Join(logs, "game.log")
	os.WriteFile(file, []byte(strings.Repeat("old line\n", 20)+"last line"), 0o644)
	log, err := ReadDiagnosticTail(file, logs, 24)
	if err != nil || !log.Truncated || !strings.Contains(log.Content, "last line") || len(log.Content) > 24 {
		t.Errorf("tail = %+v, %v", log, err)
	}
	os.WriteFile(filepath.Join(root, "outside"), []byte("PRIVATE"), 0o644)
	if _, err := ReadDiagnosticTail(filepath.Join(root, "outside"), logs, 0); err == nil || !strings.Contains(err.Error(), "Not a launcher") {
		t.Errorf("outside file: %v", err)
	}
	os.Symlink(file, filepath.Join(logs, "link"))
	if _, err := ReadDiagnosticTail(filepath.Join(logs, "link"), logs, 0); err == nil {
		t.Error("symlink must be rejected")
	}

	content := "FIRST: původní chyba\n" + strings.Repeat("diagnostika žluťoučký\n", 90000) + "LAST: dokončeno 😀\n"
	os.WriteFile(file, []byte(content), 0o644)
	preview, _ := ReadDiagnosticTail(file, logs, 0)
	if !preview.Truncated || strings.Contains(preview.Content, "FIRST:") {
		t.Error("preview should be bounded")
	}
	var copied []string
	write := func(text string) error { copied = append(copied, text); return nil }
	if err := CopyDiagnosticFile(file, logs, write, 0); err != nil || len(copied) != 1 || copied[0] != content {
		t.Errorf("full copy failed: %v", err)
	}
	os.WriteFile(file, []byte("123456789"), 0o644)
	copied = nil
	if err := CopyDiagnosticFile(file, logs, write, 8); err == nil || !strings.Contains(err.Error(), "too large for the clipboard") || copied != nil {
		t.Errorf("oversized: %v", err)
	}
	if err := CopyDiagnosticFile(file, logs, write, 9); err != nil || copied[0] != "123456789" {
		t.Errorf("exact limit: %v", err)
	}
	for _, target := range []string{filepath.Join(logs, "link"), filepath.Join(root, "outside"), logs, filepath.Join(logs, "missing")} {
		if err := CopyDiagnosticFile(target, logs, write, 0); err == nil {
			t.Errorf("copied %s", target)
		}
	}
	if err := CopyDiagnosticFile(file, logs, func(string) error { return fmt.Errorf("Clipboard unavailable") }, 0); err == nil {
		t.Error("clipboard errors must propagate")
	}
}

func TestDiagnosticLogFile(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	os.Mkdir(logs, 0o755)
	file := filepath.Join(logs, "game.log")
	os.WriteFile(file, []byte("complete file"), 0o644)
	real, _ := filepath.EvalSymlinks(file)
	if got, err := DiagnosticLogFile(file, logs); err != nil || got != real {
		t.Errorf("DiagnosticLogFile = %q, %v", got, err)
	}
	os.Symlink(file, filepath.Join(logs, "link"))
	os.Mkdir(filepath.Join(logs, "directory"), 0o755)
	os.Mkdir(filepath.Join(root, "logs-neighbor"), 0o755)
	outside := filepath.Join(root, "logs-neighbor", "private")
	os.WriteFile(outside, []byte("secret"), 0o644)
	os.Symlink(filepath.Dir(outside), filepath.Join(logs, "parent-link"))
	for target, want := range map[string]string{
		filepath.Join(logs, "link"):                   "",
		filepath.Join(logs, "directory"):              "regular file",
		outside:                                       "Not a launcher",
		filepath.Join(logs, "parent-link", "private"): "Not a launcher",
		filepath.Join(logs, "missing"):                "",
	} {
		if _, err := DiagnosticLogFile(target, logs); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("DiagnosticLogFile(%s) = %v, want %q", target, err, want)
		}
	}
}

func readRecords(t *testing.T, log *Backend) (string, []map[string]any) {
	t.Helper()
	result, err := log.Read()
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(result.Content), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid line %q", line)
		}
		records = append(records, record)
	}
	return result.Content, records
}

func intPtr(v int) *int { return &v }

func TestAccessResolutionRecords(t *testing.T) {
	log := NewBackend(t.TempDir(), nil, 0)
	for _, stage := range []string{"access-unresolved", "access-conditional", "access-special", "access-parent-unavailable", "access-cycle", "access-depth"} {
		log.Record(Record{Operation: "library", Event: "validation", Stage: stage, Skipped: intPtr(3)})
	}
	log.Record(Record{Operation: "library", Event: "validation", Stage: "CFQ7TTBZT8LN private@example.test", Skipped: intPtr(300_000)})
	content, records := readRecords(t, log)
	staged, three := 0, 0
	for _, row := range records {
		if row["stage"] != nil {
			staged++
		}
		if row["skipped"] == float64(3) {
			three++
		}
	}
	if staged != 6 || three != 6 || regexp.MustCompile(`CFQ7TTBZT8LN|private@example.test|300000`).MatchString(content) {
		t.Errorf("records: %s", content)
	}
}

func TestTicketServiceCode(t *testing.T) {
	log := NewBackend(t.TempDir(), nil, 0)
	frame := "NEXUS_TICKET_DIAGNOSTIC_V1 ticket-refused 800488AB"
	for _, operation := range []string{"library", "profile", "login", "run", "streaming"} {
		reader := NewBackendReader(operation, log.Record)
		for _, raw := range []string{frame, frame + " private-secret", strings.Replace(frame, "800488AB", "https://private.test/?token=secret", 1),
			strings.Replace(frame, "800488AB", "800488ABC", 1), strings.Replace(frame, "800488AB", "800488AZ", 1)} {
			reader.Push([]byte(raw + "\n"))
		}
	}
	log.Record(Record{Operation: "library", Event: "diagnostic", Stage: "ticket-refused", ServiceCode: "private-secret"})
	content, records := readRecords(t, log)
	var operations []string
	for _, row := range records {
		if row["serviceCode"] != nil {
			operations = append(operations, row["operation"].(string))
		}
	}
	if fmt.Sprint(operations) != "[library profile]" || regexp.MustCompile(`private-secret|private.test|token=secret`).MatchString(content) {
		t.Errorf("service codes for %v: %s", operations, content)
	}
}

func collect(operation string) (*BackendReader, *[]Record) {
	events := &[]Record{}
	return NewBackendReader(operation, func(r Record) { *events = append(*events, r) }), events
}

func pushBytewise(reader *BackendReader, text string) {
	for i := range len(text) {
		reader.Push([]byte{text[i]})
	}
}

func TestTransferAndContentStages(t *testing.T) {
	for _, operation := range []string{"streaming", "download"} {
		reader, events := collect(operation)
		pushBytewise(reader, "https://private.example/?token=SECRET\nNEXUS_TRANSFER_DIAGNOSTIC_V1 transfer-http 503\nNEXUS_TRANSFER_DIAGNOSTIC_V1 transfer-idle-timeout 0\n")
		want := []Record{{Operation: operation, Event: "diagnostic", Stage: "transfer-http", Status: 503}, {Operation: operation, Event: "diagnostic", Stage: "transfer-idle-timeout"}}
		if fmt.Sprint(*events) != fmt.Sprint(want) {
			t.Errorf("%s transfer events = %+v", operation, *events)
		}
		for _, line := range []string{"NEXUS_TRANSFER_DIAGNOSTIC_V1 unknown 0", "NEXUS_TRANSFER_DIAGNOSTIC_V1 transfer-http 999", "NEXUS_TRANSFER_DIAGNOSTIC_V1 transfer-http 403 SECRET"} {
			reader.Push([]byte(line + "\n"))
		}
		if len(*events) != 2 {
			t.Errorf("%s accepted invalid transfer lines", operation)
		}
	}
	for _, operation := range []string{"login", "library", "profile", "run", "license"} {
		reader, events := collect(operation)
		reader.Push([]byte("NEXUS_TRANSFER_DIAGNOSTIC_V1 transfer-http 403\n"))
		if len(*events) != 0 {
			t.Errorf("%s recorded a transfer stage", operation)
		}
	}
	for _, operation := range []string{"streaming", "download", "license", "run"} {
		reader, events := collect(operation)
		pushBytewise(reader, "private@example.test token=SECRET\nNEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-http 403\r\nNEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-schema 200\nNEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-device-limit 200\n")
		want := []Record{{Operation: operation, Event: "diagnostic", Stage: "content-http", Status: 403},
			{Operation: operation, Event: "diagnostic", Stage: "content-schema", Status: 200},
			{Operation: operation, Event: "diagnostic", Stage: "content-device-limit", Status: 200}}
		if fmt.Sprint(*events) != fmt.Sprint(want) {
			t.Errorf("%s content events = %+v", operation, *events)
		}
	}
	for _, operation := range []string{"profile", "login", "library"} {
		reader, events := collect(operation)
		reader.Push([]byte("NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-http 403\nNEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 content-device-limit 200\n"))
		if len(*events) != 0 {
			t.Errorf("%s recorded a content stage", operation)
		}
	}
}

func TestSessionAndLibraryStages(t *testing.T) {
	for _, operation := range []string{"library", "profile"} {
		reader, events := collect(operation)
		reader.Push([]byte("NEXUS_SESSION_DIAGNOSTIC_V1 ticket-timeout\nNEXUS_SESSION_DIAGNOSTIC_V1 ticket-retry\nNEXUS_SESSION_DIAGNOSTIC_V1 session-user-expired\n"))
		reader.Push([]byte("NEXUS_SESSION_DIAGNOSTIC_V1 private-secret\nprefix NEXUS_SESSION_DIAGNOSTIC_V1 ticket-timeout\nNEXUS_SESSION_DIAGNOSTIC_V1 ticket-timeout private-secret\n"))
		if len(*events) != 3 {
			t.Errorf("%s session events = %d", operation, len(*events))
		}
	}
	reader, events := collect("login")
	reader.Push([]byte("NEXUS_SESSION_DIAGNOSTIC_V1 ticket-timeout\n"))
	if len(*events) != 0 {
		t.Error("login must not record session stages")
	}

	reader, events = collect("library")
	pushBytewise(reader, "private-user@example.test t=SECRET&p=SECRET\nNEXUS_LIBRARY_DIAGNOSTIC_V1 entitlements-http 403 0\r\nNEXUS_CREDENTIAL_ERROR_V1 keyring-unavailable\nAccount library unavailable (stored-device); credentials and installed games are unchanged.\n")
	if len(*events) != 3 || (*events)[0].Stage != "entitlements-http" || (*events)[0].Status != 403 {
		t.Errorf("library events = %+v", *events)
	}
	for _, line := range []string{"NEXUS_CREDENTIAL_ERROR_V1 private-secret", "prefix NEXUS_CREDENTIAL_ERROR_V1 keyring-unavailable",
		"NEXUS_LIBRARY_DIAGNOSTIC_V1 entitlements-http 403 0 private-secret", "NEXUS_LIBRARY_DIAGNOSTIC_V1 private-secret 403 0",
		"NEXUS_LIBRARY_DIAGNOSTIC_V1 entitlements-http 999 0", "NEXUS_LIBRARY_DIAGNOSTIC_V1 partial 0 999999", "NEXUS_CREDENTIAL_ERROR_V1 partial",
		"NEXUS_CREDENTIAL_ERROR_V1 keyring-unavailable private-secret", strings.Repeat("x", 800) + "NEXUS_CREDENTIAL_ERROR_V1 keyring-unavailable"} {
		reader.Push([]byte(line + "\n"))
	}
	reader.Push([]byte("NEXUS_CREDENTIAL_ERROR_V1 keyring-missing-data"))
	if len(*events) != 3 {
		t.Errorf("accepted injected lines: %+v", *events)
	}
}

func TestReaderBounds(t *testing.T) {
	reader, events := collect("library")
	reader.Push([]byte(strings.Repeat("NEXUS_LIBRARY_DIAGNOSTIC_V1 partial 0 2\n", 1000)))
	if len(*events) != 32 {
		t.Errorf("events = %d, want 32", len(*events))
	}
	oversized := NewBackendReader("library", func(r Record) { *events = append(*events, r) })
	oversized.Push([]byte(strings.Repeat("x", 2*1024*1024)))
	oversized.Push([]byte("\nNEXUS_CREDENTIAL_ERROR_V1 keyring-unavailable\n"))
	if len(*events) != 32 {
		t.Error("total parsing must be bounded")
	}
}

func TestPrivateRotation(t *testing.T) {
	root := t.TempDir()
	log := NewBackend(root, func() Context { return Context{Launcher: "0.3.6", Backend: "0.2.6", Source: "managed"} }, 1024)
	for i := range 30 {
		log.Record(Record{Operation: "library", Event: "diagnostic", Stage: "partial", Skipped: intPtr(i)})
		log.Flush()
	}
	log.Record(Record{Operation: "library", Event: "diagnostic", Stage: "private-secret", Signal: "private-secret", Status: 999, Skipped: intPtr(-1)})
	log.Record(Record{Operation: "private-secret", Event: "start"})
	result, err := log.Read()
	if err != nil || !strings.Contains(result.Content, `"backend":"0.2.6"`) || strings.Contains(result.Content, "private-secret") {
		t.Fatalf("read: %v %q", err, result.Content)
	}
	dir := filepath.Join(root, "backend-diagnostics")
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode().Perm())
	}
	for _, name := range []string{"backend.log", "backend.log.1"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 || info.Size() > 1024 {
			t.Errorf("%s: %v %v", name, err, info)
		}
	}
	if persisted, _ := NewBackend(root, nil, 0).Read(); persisted.Content != result.Content {
		t.Error("persisted log differs")
	}
}

func TestUnsafeLogTargets(t *testing.T) {
	for _, kind := range []string{"file-symlink", "directory-symlink", "hardlink", "public-file"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(root, "outside")
			os.Mkdir(outside, 0o700)
			target := filepath.Join(outside, "secret")
			os.WriteFile(target, []byte("private-secret"), 0o600)
			dir := filepath.Join(root, "backend-diagnostics")
			if kind == "directory-symlink" {
				os.Symlink(outside, dir)
			} else {
				os.Mkdir(dir, 0o700)
				switch kind {
				case "file-symlink":
					os.Symlink(target, filepath.Join(dir, "backend.log"))
				case "hardlink":
					os.Link(target, filepath.Join(dir, "backend.log"))
				case "public-file":
					os.WriteFile(filepath.Join(dir, "backend.log"), []byte("private-secret"), 0o644)
					os.Chmod(filepath.Join(dir, "backend.log"), 0o644)
				}
			}
			log := NewBackend(root, nil, 0)
			log.Record(Record{Operation: "account", Event: "start"})
			if _, err := log.Read(); err == nil || !strings.Contains(err.Error(), "could not be saved") {
				t.Errorf("read: %v", err)
			}
			if data, _ := os.ReadFile(target); string(data) != "private-secret" {
				t.Error("target changed")
			}
			if _, err := NewBackend(root, nil, 0).Read(); err == nil || !strings.Contains(err.Error(), "opened safely") {
				t.Errorf("fresh read: %v", err)
			}
		})
	}
}
