package diagnostics

import (
	"fmt"
	"regexp"
	"strings"
)

// SessionStages holds fixed codes only. Never return an arbitrary backend/provider message.
var SessionStages = setOf(
	"session-user-unavailable", "session-user-expired", "session-user-invalid",
	"session-device-unavailable", "session-device-invalid", "session-device-refresh",
	"session-device-refresh-failed", "session-account-changed",
	"ticket-timeout", "ticket-connect", "ticket-network", "ticket-response", "ticket-retry", "ticket-response-received",
	"ticket-request-build", "ticket-response-parse", "ticket-response-signature", "ticket-response-crypto",
	"ticket-interaction-required", "ticket-refused",
)

var (
	ticketLine  = regexp.MustCompile(`^NEXUS_TICKET_DIAGNOSTIC_V1 (ticket-refused|ticket-interaction-required) ([A-F0-9]{8})$`)
	sessionLine = regexp.MustCompile(`^NEXUS_SESSION_DIAGNOSTIC_V1 ([a-z-]{1,60})$`)
	lineBreak   = regexp.MustCompile(`\r?\n`)
)

func setOf(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func has(set map[string]struct{}, value string) bool { _, ok := set[value]; return ok }

type TicketFailure struct{ Stage, ServiceCode string }

func TicketFailureDiagnostic(line string) (TicketFailure, bool) {
	match := ticketLine.FindStringSubmatch(line)
	if match == nil {
		return TicketFailure{}, false
	}
	return TicketFailure{match[1], match[2]}, true
}

// tail returns the last n bytes, never splitting a UTF-8 sequence.
func tail(value string, n int) string {
	if len(value) <= n {
		return value
	}
	start := len(value) - n
	for start < len(value) && value[start]&0xC0 == 0x80 {
		start++
	}
	return value[start:]
}

// SessionFailureMessage maps the last session diagnostic in output to fixed UI text, or "".
func SessionFailureMessage(output string) string {
	var last, serviceCode string
	for _, line := range lineBreak.Split(tail(output, 64_000), -1) {
		if ticket, ok := TicketFailureDiagnostic(line); ok {
			last, serviceCode = ticket.Stage, ticket.ServiceCode
			continue
		}
		match := sessionLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if match[1] == "ticket-response-received" {
			last, serviceCode = "", ""
		} else if has(SessionStages, match[1]) && match[1] != "ticket-retry" && match[1] != "session-device-refresh" {
			last, serviceCode = match[1], ""
		}
	}
	code := ""
	if serviceCode != "" {
		code = fmt.Sprintf(" (code 0x%s)", serviceCode)
	}
	switch last {
	case "session-user-unavailable":
		return "Stored Microsoft sign-in could not be read. Unlock the system keyring and try Refresh again; if it persists, sign in again in Settings."
	case "session-user-expired", "session-user-invalid":
		return "Your Microsoft sign-in has expired or is invalid. Sign in again in Settings. Your saved account has not been deleted."
	case "session-device-unavailable", "session-device-invalid":
		return "Microsoft device credentials are unavailable. Check that the system keyring is unlocked, then sign in again in Settings."
	case "session-device-refresh-failed":
		return "Microsoft device sign-in could not be renewed. Check your connection and try Refresh again. Your saved account has not been deleted."
	case "session-account-changed":
		return "The Microsoft account changed during this request. Refresh the library again."
	case "ticket-timeout":
		return "Microsoft Store sign-in timed out. Check your connection and try Refresh again."
	case "ticket-connect", "ticket-network":
		return "Could not connect to Microsoft Store sign-in. Check your connection and try Refresh again."
	case "ticket-response":
		return "Microsoft Store sign-in returned an unusable response. Try Refresh again; if it persists, share the backend diagnostic log."
	case "ticket-interaction-required":
		return fmt.Sprintf("Microsoft requires an interactive sign-in%s. Sign in again in Settings to complete any required account prompts. Your saved account has not been deleted.", code)
	case "ticket-refused":
		return fmt.Sprintf("Microsoft refused the stored sign-in%s. Try signing in again in Settings; if it persists, share the backend diagnostic log. Your saved account has not been deleted.", code)
	case "ticket-request-build":
		return "The stored Microsoft sign-in could not be used to build a ticket request. Sign in again in Settings; if it persists, share the backend diagnostic log."
	case "ticket-response-parse":
		return "Microsoft Store sign-in returned an unreadable response. Check for a captive portal or network filtering, then try Refresh. If it persists, share the backend diagnostic log."
	case "ticket-response-signature", "ticket-response-crypto":
		return "Microsoft Store sign-in response verification failed. Access was not granted. Try Refresh; if it persists, share the backend diagnostic log."
	}
	return ""
}

// Only fixed, backend-owned codes cross the private stderr boundary. Never show
// arbitrary backend stderr, JSON, account identifiers or provider error text.
var credentialMessages = map[string]string{
	"keyring-unavailable":        "The system keyring could not be accessed. Check that it is running and unlocked, then try again. On KDE or SteamOS, check that KDE Wallet is enabled. Use Desktop Mode to unlock it without removing its password.",
	"keyring-invalid-data":       "Stored Xodus credentials are empty or invalid. Check the system keyring; reinstalling Nexus does not repair these credentials.",
	"keyring-write-verification": "The system keyring did not return the credentials just saved. Check the keyring service before trying sign-in again.",
	"keyring-missing-data":       "Required Xodus credentials are missing. Please sign in again.",
	"device-authentication":      "Device authentication did not complete. Check the network connection and try again.",
	"credential-io":              "Stored Xodus credentials could not be read or saved.",
}

var credentialLine = regexp.MustCompile(`^NEXUS_CREDENTIAL_ERROR_V1 ([a-z-]+)\r?$`)

// LineFramer splits a byte stream into bounded ASCII lines. Lines that are too
// long or contain non-ASCII bytes are discarded, not truncated.
type LineFramer struct {
	maxLine, maxTotal int
	line              strings.Builder
	discard           bool
	total             int
}

// Push calls consume for each complete line and stops after maxTotal bytes.
func (f *LineFramer) Push(bytes []byte, consume func(string) bool) {
	for _, b := range bytes {
		f.total++
		if f.total > f.maxTotal {
			return
		}
		if b == '\n' {
			discard := f.discard
			line := f.line.String()
			f.line.Reset()
			f.discard = false
			if !discard && !consume(line) {
				return
			}
		} else if !f.discard {
			if f.line.Len() >= f.maxLine || b > 127 {
				f.line.Reset()
				f.discard = true
			} else {
				f.line.WriteByte(b)
			}
		}
	}
}

type CredentialErrorReader struct {
	framer  LineFramer
	Message string
}

func NewCredentialErrorReader() *CredentialErrorReader {
	return &CredentialErrorReader{framer: LineFramer{maxLine: 96, maxTotal: 2 * 1024 * 1024}}
}

func (r *CredentialErrorReader) Push(bytes []byte) {
	r.framer.Push(bytes, func(line string) bool {
		if r.Message == "" {
			if match := credentialLine.FindStringSubmatch(line); match != nil {
				r.Message = credentialMessages[match[1]]
			}
		}
		return true
	})
}

// trimCR mirrors line.replace(/\r$/, ”).
func trimCR(line string) string { return strings.TrimSuffix(line, "\r") }
