package chatgpt

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

var sharePath = regexp.MustCompile(`^/share/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// FetchHTML fetches only an explicitly supplied canonical public share URL.
// It carries no browser cookies or authentication. Redirects are rejected,
// including redirects to login, another share, or an internal address.
// The administrator may configure HTTPS_PROXY for the source request only.
func FetchHTML(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != "chatgpt.com" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !sharePath.MatchString(u.Path) {
		return nil, errors.New("source must be a canonical HTTPS ChatGPT share URL without credentials or query parameters")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return fetchHTML(ctx, client, u.String(), int64(DefaultLimits().MaxHTMLBytes))
}

func fetchHTML(ctx context.Context, client *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("cannot prepare share request")
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", "gpt-owu-bridge/0.1")
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("share fetch failed; check source connectivity")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("share unavailable or requires browser interaction")
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "text/html" {
		return nil, errors.New("share response is not HTML")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit || len(data) == 0 {
		return nil, errors.New("share response unreadable or exceeds size limit")
	}
	return data, nil
}
