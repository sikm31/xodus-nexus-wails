package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Helpers for untyped Store JSON, mirroring the defensive checks in catalog.ts.

func object(value any) map[string]any {
	record, _ := value.(map[string]any)
	return record
}

func list(value any) []any {
	items, _ := value.([]any)
	return items
}

// str returns the trimmed string, or "" for non-strings and blank strings.
func str(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func safeURL(value any, hosts map[string]bool) string {
	raw := str(value)
	if raw == "" || len(raw) > 8192 {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || !hosts[strings.ToLower(parsed.Hostname())] {
		return ""
	}
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String()
}

// Response is the result of one HTTP GET.
type Response struct {
	Status int
	Body   []byte
}

func (r Response) OK() bool { return r.Status >= 200 && r.Status <= 299 }

// Fetcher performs a GET request. maxBytes bounds the body.
type Fetcher func(ctx context.Context, target string, maxBytes int64) (Response, error)

var errTooLarge = errors.New("response is too large")

// HTTPFetcher fetches JSON over HTTPS without following redirects to other schemes.
func HTTPFetcher(client *http.Client) Fetcher {
	if client == nil {
		client = &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return func(ctx context.Context, target string, maxBytes int64) (Response, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return Response{}, err
		}
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return Response{}, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
		if err != nil {
			return Response{}, err
		}
		if int64(len(body)) > maxBytes {
			return Response{}, errTooLarge
		}
		return Response{Status: response.StatusCode, Body: body}, nil
	}
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

func productsURL(ids []string, market string) string {
	query := url.Values{}
	query.Set("bigIds", strings.Join(ids, ","))
	query.Set("market", market)
	query.Set("languages", "en-us,neutral")
	return "https://displaycatalog.mp.microsoft.com/v7.0/products?" + query.Encode()
}

func httpError(prefix string, status int) error {
	return fmt.Errorf("%s returned HTTP %d.", prefix, status)
}
