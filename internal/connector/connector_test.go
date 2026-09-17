package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/config"
	"github.com/ChenM0M/gpt-owu-bridge/internal/owu"
	"github.com/ChenM0M/gpt-owu-bridge/internal/storage"
	"github.com/go-oauth2/oauth2/v4/models"
)

func TestConnectorAuthenticatedDiscovery(t *testing.T) {
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			reply(w, 200, map[string]string{"version": "0.11.3", "deployment_id": "test-deployment"})
		case "/api/v1/auths/":
			if r.Header.Get("Authorization") != "Bearer synthetic" {
				w.WriteHeader(401)
				return
			}
			reply(w, 200, map[string]string{"id": "account"})
		default:
			t.Error("unexpected downstream access")
			w.WriteHeader(404)
		}
	}))
	defer downstream.Close()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	client, e := owu.New(owu.Config{BaseURL: downstream.URL, Token: "synthetic"})
	if e != nil {
		t.Fatal(e)
	}
	identity, e := client.VerifyIdentity(ctx)
	if e != nil {
		t.Fatal(e)
	}
	store, install, e := storage.Initialize(ctx, filepath.Join(dir, storage.DefaultDatabaseFilename), storage.TargetIdentity{OWUSiteID: identity.SiteID, OWUAccountID: identity.AccountID})
	if e != nil {
		t.Fatal(e)
	}
	store.Close()
	cfg, e := config.Load(nil, []string{"GATE_DATA_DIR=" + dir, "GATE_OWU_BASE_URL=" + downstream.URL, "GATE_OWU_TOKEN=synthetic", "GATE_PUBLIC_URL=https://bridge.example/bridge"})
	if e != nil {
		t.Fatal(e)
	}
	h, close, e := New(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	call := func(body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://bridge.example/bridge/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	if w := call(body, ""); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("missing challenge %d", w.Code)
	}
	ts, e := openTokens(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer ts.db.Close()
	token := &models.Token{Access: "synthetic-access", UserID: install.OwnerID, Scope: scope, AccessCreateAt: time.Now(), AccessExpiresIn: time.Minute, Extension: url.Values{"resource": {"https://bridge.example/bridge/mcp"}}}
	if e = ts.Create(ctx, token); e != nil {
		t.Fatal(e)
	}
	w := call(body, "synthetic-access")
	if w.Code != 200 {
		t.Fatalf("tools/list %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if e = json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	if len(response.Result.Tools) != 3 {
		t.Fatalf("tools missing: %s", w.Body.String())
	}
	for _, path := range []string{"/bridge/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server/bridge"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://bridge.example"+path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://bridge.example/bridge/mcp", strings.NewReader(body))
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("untrusted origin allowed")
	}
}
