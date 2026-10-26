// Package auth ports auth-protocol.ts, auth-transport.ts, auth-fallback.ts,
// auth-cdp.ts and auth-browser.ts: the backend's external sign-in protocol
// and the views that host Microsoft sign-in pages.
package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Private child-process transport. Never send frames to logs or the renderer UI.
const (
	Prefix     = "NEXUS_AUTH_V1 "
	FrameLimit = 128 * 1024
)

var Hosts = []string{"login.live.com", "account.live.com", "login.microsoftonline.com", "login.microsoft.com", "account.microsoft.com", "sisu.xboxlive.com"}

var headerNames = []string{"cxh-capabilities", "cxh-correlationid", "cxh-msabinaryversion", "cxh-identityclientbinaryversion", "cxh-osversioninfo",
	"cxh-platform", "cxh-protocol", "cxh-source", "hostapp"}

type FrameType string

const (
	Ready   FrameType = "ready"
	Open    FrameType = "open"
	Close   FrameType = "close"
	Context FrameType = "context"
)

type Frame struct {
	Type    FrameType
	Session int64
	URL     string
	Headers map[string]string
	Context string
}

func TrustedURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Port() == "" && slices.Contains(Hosts, parsed.Hostname()) &&
		parsed.Host == parsed.Hostname()
}

var printable = regexp.MustCompile(`^[\x20-\x7e]*$`)

// ParseFrame returns nil for a line without the protocol prefix.
func ParseFrame(line string) (*Frame, error) {
	if !strings.HasPrefix(line, Prefix) {
		return nil, nil
	}
	if len(line) > FrameLimit {
		return nil, errors.New("Sign-in response exceeds the protocol limit.")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line[len(Prefix):]), &raw); err != nil || raw == nil {
		return nil, errors.New("Invalid sign-in protocol response.")
	}
	var kind string
	json.Unmarshal(raw["type"], &kind)
	if kind == "ready" && strings.TrimSpace(string(raw["version"])) == "1" {
		return &Frame{Type: Ready}, nil
	}
	var session float64
	if json.Unmarshal(raw["session"], &session) != nil || session != math.Trunc(session) || session < 1 || session > 1<<53-1 {
		return nil, errors.New("Invalid sign-in session.")
	}
	id := int64(session)
	switch kind {
	case "close":
		return &Frame{Type: Close, Session: id}, nil
	case "context":
		var context string
		if bytes.HasPrefix(bytes.TrimSpace(raw["context"]), []byte(`"`)) && json.Unmarshal(raw["context"], &context) == nil && len(context) <= 4096 {
			return &Frame{Type: Context, Session: id, Context: context}, nil
		}
	case "open":
		var target string
		var headers map[string]json.RawMessage
		if json.Unmarshal(raw["url"], &target) == nil && TrustedURL(target) && len(target) <= 32_768 &&
			bytes.HasPrefix(bytes.TrimSpace(raw["headers"]), []byte("{")) && json.Unmarshal(raw["headers"], &headers) == nil {
			if len(headers) > len(headerNames) {
				return nil, errors.New("Invalid sign-in request headers.")
			}
			result := map[string]string{}
			for name, rawValue := range headers {
				lower := strings.ToLower(name)
				var value string
				if !slices.Contains(headerNames, lower) || !bytes.HasPrefix(bytes.TrimSpace(rawValue), []byte(`"`)) || json.Unmarshal(rawValue, &value) != nil ||
					len(value) > 4096 || !printable.MatchString(value) {
					return nil, errors.New("Invalid sign-in request headers.")
				}
				if _, duplicate := result[lower]; duplicate {
					return nil, errors.New("Invalid sign-in request headers.")
				}
				result[lower] = value
			}
			return &Frame{Type: Open, Session: id, URL: target, Headers: result}, nil
		}
	}
	return nil, errors.New("Unsupported sign-in protocol response.")
}

// ContextMessage is the CloudExperienceHost callback the sign-in page expects.
func ContextMessage(context string) string {
	message, _ := json.Marshal(map[string]any{"type": "callback", "value": map[string]any{"name": "CloudExperienceHost.getContext",
		"args": []string{"CloudExperienceHost", "TokenBroker", "TokenBroker", `{"PrivatePropertyBag":1,"PasswordlessConnect":1,"PreferAssociate":1,"ChromelessUI":0}`}, "context": context}})
	quoted, _ := json.Marshal(string(message))
	return `globalThis["CloudExperienceHost.Bridge.dispatchMessage"](` + string(quoted) + `)`
}
