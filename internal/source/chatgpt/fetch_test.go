package chatgpt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchRejectsNonCanonicalSources(t *testing.T) {
	for _, address := range []string{
		"http://chatgpt.com/share/12345678-1234-1234-1234-123456789abc",
		"https://chatgpt.com.evil.test/share/12345678-1234-1234-1234-123456789abc",
		"https://user:secret@chatgpt.com/share/12345678-1234-1234-1234-123456789abc",
		"https://chatgpt.com:443/share/12345678-1234-1234-1234-123456789abc",
		"https://chatgpt.com/share/12345678-1234-1234-1234-123456789abc?token=secret",
		"https://127.0.0.1/private", "https://chatgpt.com/c/private",
		"https://chatgpt.com/share/%31%32%33", "https://chatgpt.com/share/../private",
	} {
		if _, err := FetchHTML(context.Background(), address); err == nil {
			t.Errorf("accepted invalid source")
		}
	}
}

func TestFetchHTTPBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		ct, body string
		limit    int64
		ok       bool
	}{
		{"html", 200, "text/html; charset=utf-8", "<html>test</html>", 100, true},
		{"login redirect", 302, "text/html", "redirect", 100, false},
		{"challenge", 403, "text/html", "challenge", 100, false},
		{"wrong type", 200, "application/json", "{}", 100, false},
		{"oversize", 200, "text/html", "123456", 5, false},
		{"empty", 200, "text/html", "", 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("unexpected credentials")
				}
				w.Header().Set("Content-Type", tc.ct)
				w.Header().Set("Location", "http://127.0.0.1/private")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			b, err := fetchHTML(context.Background(), client, server.URL, tc.limit)
			if (err == nil) != tc.ok {
				t.Fatalf("unexpected success=%v", err == nil)
			}
			if tc.ok && string(b) != tc.body {
				t.Error("HTML changed")
			}
		})
	}
}
