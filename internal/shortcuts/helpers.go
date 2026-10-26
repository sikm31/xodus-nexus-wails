package shortcuts

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"xodus-nexus-wails/internal/launcherenv"
)

const maxIconBytes = 4 * 1024 * 1024

var iconHosts = map[string]bool{"store-images.s-microsoft.com": true, "store-images.microsoft.com": true}
var dataURL = regexp.MustCompile(`^data:image/(?:png|jpeg|webp);base64,([a-zA-Z0-9+/=]+)$`)

func raster(data []byte) bool {
	return bytes.HasPrefix(data, pngMagic) || bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) ||
		(len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP")
}

// IconFetch performs a GET without following redirects.
type IconFetch func(ctx context.Context, url string) (*http.Response, error)

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect") }}

func HTTPIconFetch(ctx context.Context, target string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return noRedirects.Do(request)
}

// LoadIcon returns optional artwork: network/decoder failures never prevent a shortcut.
func LoadIcon(coverDataURL, coverURL string, normalize func([]byte) []byte, fetch IconFetch) []byte {
	if fetch == nil {
		fetch = HTTPIconFetch
	}
	convert := func(data []byte) (out []byte) {
		defer func() {
			if recover() != nil {
				out = nil
			}
		}()
		if len(data) > maxIconBytes || !raster(data) {
			return nil
		}
		return normalize(data)
	}
	if coverDataURL != "" && float64(len(coverDataURL)) <= maxIconBytes*1.4 {
		if match := dataURL.FindStringSubmatch(coverDataURL); match != nil {
			if decoded, err := base64.StdEncoding.DecodeString(match[1]); err == nil {
				if local := convert(decoded); len(local) > 0 {
					return local
				}
			}
		}
	}
	if coverURL == "" {
		return nil
	}
	parsed, err := url.Parse(coverURL)
	if err != nil || parsed.Scheme != "https" || !iconHosts[strings.ToLower(parsed.Hostname())] || parsed.Port() != "" || parsed.User != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := fetch(ctx, parsed.String())
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil
	}
	if declared := response.Header.Get("Content-Length"); declared != "" {
		if size, err := strconv.ParseInt(declared, 10, 64); err != nil || size > maxIconBytes {
			return nil
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxIconBytes+1))
	if err != nil || len(data) > maxIconBytes {
		return nil
	}
	return convert(data)
}

// Launch serializes launch preparation, coalesces duplicate shortcuts and
// never queues games.
type Launch struct {
	active   func() string
	launch   func(id string) error
	mu       sync.Mutex
	starting string
}

func NewLaunch(active func() string, launch func(string) error) *Launch {
	return &Launch{active: active, launch: launch}
}

func (l *Launch) Request(id string) error {
	l.mu.Lock()
	if l.starting == id || l.active() == id {
		l.mu.Unlock()
		return nil
	}
	if l.starting != "" || l.active() != "" {
		l.mu.Unlock()
		return errors.New("Another game is already starting or running.")
	}
	l.starting = id
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.starting = ""
		l.mu.Unlock()
	}()
	return l.launch(id)
}

func (l *Launch) Preparing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starting != ""
}

// OpenDesktopTarget opens a file, folder or URL with the system application,
// using the user's original environment rather than launcher-only settings.
func OpenDesktopTarget(target string) error {
	cmd := exec.Command("xdg-open", target)
	cmd.Env = launcherenv.External(nil).List()
	if err := cmd.Run(); err != nil {
		return errors.New("Could not open this item with the system application.")
	}
	return nil
}
