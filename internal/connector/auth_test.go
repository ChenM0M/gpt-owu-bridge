package connector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-oauth2/oauth2/v4/models"
)

func testAuth(t *testing.T) *authorization {
	t.Helper()
	s, e := openTokens(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.db.Close() })
	a := newAuthorization(s, "https://bridge.example/bridge", "owner", "account", "http://owu.invalid")
	if e = s.addClient(context.Background(), &models.Client{ID: "client", Domain: "https://chatgpt.com/connector_platform_oauth_redirect", Public: true}); e != nil {
		t.Fatal(e)
	}
	return a
}
func codeFor(t *testing.T, a *authorization, verifier string) string {
	t.Helper()
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {"client"}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "scope": {scope}, "resource": {a.resource}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	r := httptest.NewRequest("GET", a.base+"/authorize?"+q.Encode(), nil)
	r.ParseForm()
	ar, e := a.server.ValidationAuthorizeRequest(r)
	if e != nil {
		t.Fatal(e)
	}
	ar.UserID = "owner"
	tok, e := a.server.GetAuthorizeToken(r.Context(), ar)
	if e != nil {
		t.Fatal(e)
	}
	return tok.GetCode()
}
func exchange(a *authorization, q url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", a.base+"/token", strings.NewReader(q.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.token(w, r)
	return w
}
func TestOAuthPKCEAudiencePersistenceRotationAndRevocation(t *testing.T) {
	a := testAuth(t)
	verifier := strings.Repeat("a", 43)
	code := codeFor(t, a, verifier)
	q := url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "resource": {a.resource}, "code": {code}, "code_verifier": {verifier}}
	wrong := url.Values{}
	for k, v := range q {
		wrong[k] = append([]string(nil), v...)
	}
	wrong.Set("resource", "https://attacker.invalid/mcp")
	if w := exchange(a, wrong); w.Code != 400 {
		t.Fatal("wrong audience accepted")
	}
	w := exchange(a, q)
	if w.Code != 200 {
		t.Fatalf("exchange status %d: %s", w.Code, w.Body.String())
	}
	var tok map[string]any
	json.Unmarshal(w.Body.Bytes(), &tok)
	if w = exchange(a, q); w.Code == 200 {
		t.Fatal("code replay accepted")
	}
	access := tok["access_token"].(string)
	refresh := tok["refresh_token"].(string)
	protect := a.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	check := func(token string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", a.resource, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		protect.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("protected got %d want %d", w.Code, want)
		}
	}
	check(access, 204)
	check("bad", 401)
	stored, e := a.store.GetByAccess(context.Background(), access)
	if e != nil || stored.GetUserID() != "owner" {
		t.Fatal("not persisted")
	}
	// Reload from the on-disk database with a fresh manager.
	again := newAuthorization(a.store, a.base, "owner", "account", a.owu)
	if _, e = again.manager.LoadAccessToken(context.Background(), access); e != nil {
		t.Fatal(e)
	}
	if e = a.store.addClient(context.Background(), &models.Client{ID: "other", Public: true, Domain: "https://chatgpt.com/connector_platform_oauth_redirect"}); e != nil {
		t.Fatal(e)
	}
	if bad := exchange(a, url.Values{"grant_type": {"refresh_token"}, "client_id": {"other"}, "resource": {a.resource}, "refresh_token": {refresh}}); bad.Code == 200 {
		t.Fatal("cross-client refresh accepted")
	}
	w = exchange(a, url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "resource": {a.resource}, "refresh_token": {refresh}})
	if w.Code != 200 {
		t.Fatalf("refresh: %s", w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &tok)
	check(access, 401)
	check(tok["access_token"].(string), 204)
	rev := httptest.NewRequest("POST", a.base+"/revoke", strings.NewReader(url.Values{"client_id": {"client"}, "token": {tok["refresh_token"].(string)}}.Encode()))
	rev.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.revoke(httptest.NewRecorder(), rev)
	check(tok["access_token"].(string), 401)
}
func TestBadPKCEAndExpiredTokens(t *testing.T) {
	a := testAuth(t)
	code := codeFor(t, a, strings.Repeat("a", 43))
	w := exchange(a, url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "resource": {a.resource}, "code": {code}, "code_verifier": {strings.Repeat("b", 43)}})
	if w.Code == 200 {
		t.Fatal("bad PKCE accepted")
	}
	tok := &models.Token{ClientID: "client", UserID: "owner", Scope: scope, Access: "expired", AccessCreateAt: time.Now().Add(-time.Hour), AccessExpiresIn: time.Minute, Extension: url.Values{"resource": {a.resource}}}
	if e := a.store.Create(context.Background(), tok); e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", a.resource, nil)
	r.Header.Set("Authorization", "Bearer expired")
	w = httptest.NewRecorder()
	a.protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("expired accepted") })).ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestRedirectAllowlist(t *testing.T) {
	for _, u := range []string{"https://chatgpt.com.evil/connector/oauth/a", "http://chatgpt.com/connector/oauth/a", "https://chatgpt.com/connector/oauth/a?next=evil", "https://chatgpt.com/connector/oauth/../x", "https://evil.invalid/callback"} {
		if validRedirect(u) {
			t.Fatalf("allowed %s", u)
		}
	}
}
func TestConsentRequiresOwnerAndCSRF(t *testing.T) {
	a := testAuth(t)
	r := httptest.NewRequest("POST", a.base+"/authorize?response_type=code&client_id=client&redirect_uri=https%3A%2F%2Fchatgpt.com%2Fconnector_platform_oauth_redirect&scope=bridge%3Async&resource="+url.QueryEscape(a.resource)+"&code_challenge_method=S256&code_challenge="+strings.Repeat("a", 43), strings.NewReader("decision=allow"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.authorize(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestRefreshReplayRevokesFamily(t *testing.T) {
	a := testAuth(t)
	verifier := strings.Repeat("a", 43)
	code := codeFor(t, a, verifier)
	w := exchange(a, url.Values{"grant_type": {"authorization_code"}, "client_id": {"client"}, "redirect_uri": {"https://chatgpt.com/connector_platform_oauth_redirect"}, "resource": {a.resource}, "code": {code}, "code_verifier": {verifier}})
	var first, second map[string]string
	var decode map[string]any
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	json.Unmarshal(w.Body.Bytes(), &decode)
	first = map[string]string{"refresh": decode["refresh_token"].(string)}
	q := url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "resource": {a.resource}, "refresh_token": {first["refresh"]}}
	w = exchange(a, q)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &decode)
	second = map[string]string{"access": decode["access_token"].(string), "refresh": decode["refresh_token"].(string)}
	if w = exchange(a, q); w.Code == 200 {
		t.Fatal("replay accepted")
	}
	if _, e := a.manager.LoadAccessToken(context.Background(), second["access"]); e == nil {
		t.Fatal("replayed token family still valid")
	}
}
