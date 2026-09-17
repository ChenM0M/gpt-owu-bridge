package connector

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	oauth "github.com/go-oauth2/oauth2/v4"
	oe "github.com/go-oauth2/oauth2/v4/errors"
	"github.com/go-oauth2/oauth2/v4/manage"
	"github.com/go-oauth2/oauth2/v4/models"
	"github.com/go-oauth2/oauth2/v4/server"
)

const scope = "bridge:sync"

type authorization struct {
	mu                                                sync.Mutex
	store                                             *tokenStore
	manager                                           *manage.Manager
	server                                            *server.Server
	base, resource, origin, path, owner, account, owu string
	client                                            *http.Client
}

func randomID() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func newAuthorization(store *tokenStore, base, owner, account, owu string) *authorization {
	u, _ := url.Parse(base)
	a := &authorization{store: store, base: base, resource: base + "/mcp", origin: u.Scheme + "://" + u.Host, path: u.Path, owner: owner, account: account, owu: owu, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect disabled") }}}
	m := manage.NewDefaultManager()
	m.MapClientStorage(store)
	m.MapTokenStorage(store)
	m.SetAuthorizeCodeTokenCfg(&manage.Config{AccessTokenExp: 15 * time.Minute, RefreshTokenExp: 30 * 24 * time.Hour, IsGenerateRefresh: true})
	m.SetRefreshTokenCfg(&manage.RefreshingConfig{AccessTokenExp: 15 * time.Minute, RefreshTokenExp: 30 * 24 * time.Hour, IsGenerateRefresh: true, IsResetRefreshTime: false, IsRemoveAccess: true, IsRemoveRefreshing: true})
	m.SetValidateURIHandler(func(expected, actual string) error {
		if expected != actual {
			return oe.ErrInvalidRedirectURI
		}
		return nil
	})
	m.SetExtractExtensionHandler(func(_ *oauth.TokenGenerateRequest, t oauth.ExtendableTokenInfo) {
		v := t.GetExtension()
		if v == nil {
			v = url.Values{}
		}
		v.Set("resource", a.resource)
		if v.Get("family") == "" {
			v.Set("family", randomID())
		}
		t.SetExtension(v)
	})
	s := server.NewServer(&server.Config{TokenType: "Bearer", AllowedResponseTypes: []oauth.ResponseType{oauth.Code}, AllowedGrantTypes: []oauth.GrantType{oauth.AuthorizationCode, oauth.Refreshing}, AllowedCodeChallengeMethods: []oauth.CodeChallengeMethod{oauth.CodeChallengeS256}, ForcePKCE: true}, m)
	s.ClientInfoHandler = server.ClientFormHandler
	s.ClientScopeHandler = func(t *oauth.TokenGenerateRequest) (bool, error) { return t.Scope == scope, nil }
	s.RefreshingScopeHandler = func(t *oauth.TokenGenerateRequest, old string) (bool, error) {
		return old == scope && (t.Scope == "" || t.Scope == scope), nil
	}
	s.ExtensionFieldsHandler = func(oauth.TokenInfo) map[string]interface{} { return map[string]interface{}{"resource": a.resource} }
	a.manager = m
	a.server = s
	return a
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (a *authorization) metadata(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"issuer": a.base, "authorization_endpoint": a.base + "/authorize", "token_endpoint": a.base + "/token", "registration_endpoint": a.base + "/register", "revocation_endpoint": a.base + "/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": []string{scope}, "authorization_response_iss_parameter_supported": true})
}
func (a *authorization) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	reply(w, 200, map[string]any{"resource": a.resource, "authorization_servers": []string{a.base}, "scopes_supported": []string{scope}, "bearer_methods_supported": []string{"header"}})
}
func validRedirect(s string) bool {
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Host != "chatgpt.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path == "/connector_platform_oauth_redirect" {
		return true
	}
	tail := strings.TrimPrefix(u.Path, "/connector/oauth/")
	return tail != u.Path && len(tail) > 0 && len(tail) < 128 && !strings.ContainsAny(tail, "/\\.")
}
func (a *authorization) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RedirectURIs  []string `json:"redirect_uris"`
		AuthMethod    string   `json:"token_endpoint_auth_method"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in) != nil || len(in.RedirectURIs) != 1 || !validRedirect(in.RedirectURIs[0]) || (in.AuthMethod != "" && in.AuthMethod != "none") {
		reply(w, 400, map[string]string{"error": "invalid_client_metadata"})
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			reply(w, 400, map[string]string{"error": "invalid_client_metadata"})
			return
		}
	}
	for _, t := range in.ResponseTypes {
		if t != "code" {
			reply(w, 400, map[string]string{"error": "invalid_client_metadata"})
			return
		}
	}
	c := &models.Client{ID: randomID(), Domain: in.RedirectURIs[0], Public: true}
	a.mu.Lock()
	e := a.store.addClient(r.Context(), c)
	a.mu.Unlock()
	if e != nil {
		reply(w, 429, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	reply(w, 201, map[string]any{"client_id": c.ID, "redirect_uris": in.RedirectURIs, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": scope})
}

var consent = template.Must(template.New("consent").Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>授权 GPT OWU Bridge</title><body><main><h1>授权 GPT OWU Bridge</h1><p>允许 ChatGPT 预览分享链接，并在你确认后同步到当前 OWU 账号。桥接只管理自己创建的对话。</p><p>接收授权的客户端：ChatGPT（{{.Redirect}}）</p><form method="post"><input type="hidden" name="csrf" value="{{.CSRF}}"><button type="submit" name="decision" value="allow">允许连接</button><button type="submit" name="decision" value="deny">取消</button></form></main></body></html>`))

func (a *authorization) ownerLoggedIn(r *http.Request) bool {
	// Only the OWU session cookie is forwarded to the fixed local OWU origin.
	c, e := r.Cookie("token")
	if e != nil || len(c.Value) > 16<<10 {
		return false
	}
	q, e := http.NewRequestWithContext(r.Context(), "GET", a.owu+"/api/v1/auths/", nil)
	if e != nil {
		return false
	}
	q.AddCookie(c)
	res, e := a.client.Do(q)
	if e != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return false
	}
	var v struct {
		ID string `json:"id"`
	}
	return json.NewDecoder(io.LimitReader(res.Body, 128<<10)).Decode(&v) == nil && v.ID == a.account
}
func (a *authorization) authorize(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		http.Error(w, "Invalid request", 400)
		return
	}
	q := r.URL.Query()
	// Use query-only protocol parameters; POST body is only the consent decision.
	if q.Get("resource") != a.resource || q.Get("scope") != scope || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || len(q.Get("state")) > 4096 {
		http.Error(w, "Invalid OAuth request", 400)
		return
	}
	a.mu.Lock()
	c, e := a.store.GetByID(r.Context(), q.Get("client_id"))
	a.mu.Unlock()
	if e != nil || c.GetDomain() != q.Get("redirect_uri") {
		http.Error(w, "Invalid client or redirect", 400)
		return
	}
	if !a.ownerLoggedIn(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(401)
		io.WriteString(w, `<h1>请先登录 OWU</h1><p>请在另一个标签页登录本站 OWU，然后刷新此授权页。仅部署者账号可授权。</p><a href="/auth" target="_blank" rel="noopener">打开 OWU 登录页</a>`)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; frame-ancestors 'none'")
	if r.Method == "GET" {
		csrf := randomID()
		http.SetCookie(w, &http.Cookie{Name: "bridge_csrf", Value: csrf, Path: a.path + "/authorize", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 600})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = consent.Execute(w, map[string]string{"CSRF": csrf, "Redirect": c.GetDomain()})
		return
	}
	cookie, e := r.Cookie("bridge_csrf")
	if e != nil || r.Header.Get("Origin") != a.origin || len(r.PostForm.Get("csrf")) < 32 || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.PostForm.Get("csrf"))) != 1 {
		http.Error(w, "Consent expired; reload and retry", 403)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "bridge_csrf", Path: a.path + "/authorize", Secure: true, HttpOnly: true, MaxAge: -1})
	target, _ := url.Parse(c.GetDomain())
	values := target.Query()
	values.Set("state", q.Get("state"))
	values.Set("iss", a.base)
	if r.PostForm.Get("decision") != "allow" {
		values.Set("error", "access_denied")
	} else {
		req := r.Clone(r.Context())
		req.Form = q
		ar, e := a.server.ValidationAuthorizeRequest(req)
		if e != nil {
			http.Error(w, "Invalid authorization request", 400)
			return
		}
		ar.UserID = a.owner
		a.mu.Lock()
		tok, e := a.server.GetAuthorizeToken(r.Context(), ar)
		a.mu.Unlock()
		if e != nil {
			http.Error(w, "Authorization failed", 400)
			return
		}
		values.Set("code", tok.GetCode())
	}
	target.RawQuery = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}
func (a *authorization) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil || r.Form.Get("resource") != a.resource {
		reply(w, 400, map[string]string{"error": "invalid_target"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, e := a.store.db.BeginTx(r.Context(), nil)
	if e != nil {
		reply(w, 503, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	a.store.tx = tx
	defer func() { a.store.tx = nil; _ = tx.Rollback() }()
	if r.Form.Get("grant_type") == "refresh_token" {
		old, err := a.store.GetByRefresh(r.Context(), r.Form.Get("refresh_token"))
		if err != nil {
			var family string
			if a.store.query().QueryRowContext(r.Context(), "SELECT family FROM spent_refresh WHERE hash=? AND client=?", digest(r.Form.Get("refresh_token")), r.Form.Get("client_id")).Scan(&family) == nil && family != "" {
				if _, err = a.store.query().ExecContext(r.Context(), "DELETE FROM tokens WHERE json_extract(data,'$.Extension.family[0]')=?", family); err == nil {
					_ = tx.Commit()
				}
			}
			reply(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		if err != nil || old.GetClientID() != r.Form.Get("client_id") || old.GetUserID() != a.owner || old.GetScope() != scope {
			reply(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		ext, ok := old.(oauth.ExtendableTokenInfo)
		if !ok || ext.GetExtension().Get("resource") != a.resource {
			reply(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
	}
	buffer := &bufferedResponse{header: make(http.Header)}
	if e = a.server.HandleTokenRequest(buffer, r); e != nil {
		reply(w, 400, map[string]string{"error": "invalid_grant"})
		return
	}
	// Commit even protocol rejection: failed PKCE consumes its authorization code.
	if e = tx.Commit(); e != nil {
		reply(w, 503, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	for key, values := range buffer.header {
		w.Header()[key] = values
	}
	status := buffer.status
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = w.Write(buffer.body.Bytes())
}
func (a *authorization) revoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		w.WriteHeader(400)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	value := r.Form.Get("token")
	t, e := a.store.GetByRefresh(r.Context(), value)
	if e != nil {
		t, e = a.store.GetByAccess(r.Context(), value)
	}
	if e == nil && t.GetClientID() == r.Form.Get("client_id") {
		_ = a.store.RemoveByAccess(r.Context(), t.GetAccess())
		_ = a.store.RemoveByRefresh(r.Context(), t.GetRefresh())
	}
	w.WriteHeader(200)
}
func (a *authorization) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, "Bearer ") && len(h) < 8192 {
			a.mu.Lock()
			t, e := a.manager.LoadAccessToken(r.Context(), strings.TrimPrefix(h, "Bearer "))
			a.mu.Unlock()
			if e == nil && t.GetUserID() == a.owner && t.GetScope() == scope {
				if ext, ok := t.(oauth.ExtendableTokenInfo); ok && ext.GetExtension().Get("resource") == a.resource {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+a.base+`/.well-known/oauth-protected-resource", scope="`+scope+`"`)
		reply(w, 401, map[string]string{"error": "unauthorized"})
	})
}

type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *bufferedResponse) Header() http.Header { return w.header }
func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}
