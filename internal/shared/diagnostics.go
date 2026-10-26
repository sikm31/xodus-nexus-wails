package shared

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic is a strict stage/status record parsed from backend stderr.
type Diagnostic struct {
	Stage  string
	Status int // 0 when absent
}

// ContentLicenseStages is fixed content-licensing metadata only, never provider messages or license data.
var ContentLicenseStages = setOf(
	"content-timeout", "content-request", "content-http", "content-size", "content-read",
	"content-json", "content-schema", "content-not-entitled", "content-device-limit", "content-empty-keys",
	"content-base64", "content-utf8", "content-xml", "content-license-blob", "content-parsed",
)

// TransferStages is strict metadata from the downloader, never raw CDN URLs or provider messages.
var TransferStages = setOf(
	"transfer-headers-timeout", "transfer-idle-timeout", "transfer-request", "transfer-read",
	"transfer-eof", "transfer-http", "transfer-range", "transfer-encoding", "transfer-size",
	"transfer-io", "transfer-cancelled",
	"metadata-http", "metadata-headers-timeout", "metadata-request", "metadata-encoding", "metadata-invalid",
)

var (
	contentLine  = regexp.MustCompile(`^NEXUS_CONTENT_LICENSE_DIAGNOSTIC_V1 ([a-z0-9-]{1,60}) (0|[1-5][0-9]{2})$`)
	transferLine = regexp.MustCompile(`^NEXUS_TRANSFER_DIAGNOSTIC_V1 ([a-z-]{1,50}) (0|[1-5][0-9]{2})$`)
)

func setOf(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// utf16Len matches JavaScript string length for the bounded-line checks.
func utf16Len(value string) int {
	n := 0
	for _, r := range value {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func parseStageLine(pattern *regexp.Regexp, stages map[string]struct{}, line string) (Diagnostic, bool) {
	if utf16Len(line) > 128 || strings.ContainsAny(line, "\r\n") {
		return Diagnostic{}, false
	}
	match := pattern.FindStringSubmatch(line)
	if match == nil {
		return Diagnostic{}, false
	}
	if _, ok := stages[match[1]]; !ok {
		return Diagnostic{}, false
	}
	status, _ := strconv.Atoi(match[2])
	return Diagnostic{Stage: match[1], Status: status}, true
}

func ContentLicenseDiagnostic(line string) (Diagnostic, bool) {
	return parseStageLine(contentLine, ContentLicenseStages, line)
}

func TransferDiagnostic(line string) (Diagnostic, bool) {
	return parseStageLine(transferLine, TransferStages, line)
}

func lastLines(log []string, n int) []string {
	if len(log) > n {
		return log[len(log)-n:]
	}
	return log
}

func ContentLicenseFailure(log []string) string {
	var last *Diagnostic
	for _, line := range lastLines(log, 256) {
		if record, ok := ContentLicenseDiagnostic(line); ok {
			if record.Stage == "content-parsed" {
				last = nil
			} else {
				last = &record
			}
		}
	}
	if last == nil {
		return ""
	}
	switch last.Stage {
	case "content-device-limit":
		return "Microsoft Store device limit reached. Open https://account.microsoft.com/devices/content, unlink an old device you no longer use, then try again. You do not need to sign out or reset the wallet."
	case "content-http":
		switch {
		case last.Status == 401:
			return "Microsoft rejected content licensing sign-in (HTTP 401). Sign in again in Settings."
		case last.Status == 403:
			return "Microsoft denied this content-license request (HTTP 403). Check the account, purchased edition and Store region."
		case last.Status == 429:
			return "Microsoft content licensing is rate-limiting requests (HTTP 429). Wait before trying again."
		case last.Status >= 500:
			return fmt.Sprintf("Microsoft content licensing returned a server error (HTTP %d). Try again later.", last.Status)
		}
		return "Microsoft content licensing returned an unexpected HTTP status. Share the backend diagnostic log."
	case "content-timeout":
		return "Microsoft content licensing timed out. Check your connection and try again."
	case "content-request", "content-read":
		return "The Microsoft content-license request could not be completed. Check your connection and try again."
	case "content-not-entitled":
		return "Microsoft did not grant a license for this account and package. Check the purchased edition, subscription and Store region."
	case "content-size":
		return "The content-license response exceeded the safety limit. Share the backend diagnostic log."
	}
	return "Microsoft returned an invalid or unsupported content-license response. Share the backend diagnostic log."
}

func TransferFailure(log []string) string {
	var record *Diagnostic
	for _, line := range lastLines(log, 256) {
		if next, ok := TransferDiagnostic(line); ok && next.Stage != "transfer-cancelled" {
			record = &next
		}
	}
	if record == nil {
		return ""
	}
	const resume = "Partial files are kept. Use Retry installation to refresh the package link and resume."
	switch record.Stage {
	case "metadata-http", "transfer-http":
		if record.Status == 401 || record.Status == 403 {
			return fmt.Sprintf("The package download link was rejected (HTTP %d). %s", record.Status, resume)
		}
		if record.Status == 429 {
			return "The download server is rate-limiting requests (HTTP 429). Wait before retrying. " + resume
		}
		status := ""
		if record.Status != 0 {
			status = fmt.Sprintf(" (HTTP %d)", record.Status)
		}
		return fmt.Sprintf("The download server returned an unexpected HTTP response%s. %s", status, resume)
	case "metadata-headers-timeout", "transfer-headers-timeout":
		return "The download server did not respond in time after repeated attempts. " + resume
	case "transfer-idle-timeout":
		return "The download stopped receiving data after repeated attempts. " + resume
	case "metadata-request", "transfer-request", "transfer-read", "transfer-eof":
		return "The download connection failed or ended before all data arrived. " + resume
	case "metadata-encoding", "transfer-range", "transfer-encoding", "transfer-size":
		return "The server returned an invalid package byte range or encoding. Unsafe data was not accepted. " + resume
	case "metadata-invalid":
		return "The package metadata could not be read or is invalid. " + resume
	case "transfer-io":
		return "The download could not read or write its files or checkpoints. Check free space and folder permissions. " + resume
	}
	return ""
}
