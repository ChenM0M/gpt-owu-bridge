package owu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

const testToken = "canary-super-secret-token"

func TestVerifyIdentityUsesExactContractAndBindsURL(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/version":
			if r.Header.Get("Authorization") != "" {
				t.Error("version request must not carry the bearer credential")
			}
			writeJSON(t, w, map[string]any{"version": "0.11.3", "deployment_id": "deploy-1"})
		case "/api/v1/auths/":
			if r.Header.Get("Authorization") != "Bearer "+testToken {
				t.Error("identity request did not use the configured credential")
			}
			writeJSON(t, w, map[string]any{
				"id": "account-1", "email": "owner@example.test", "name": "Owner", "role": "user",
				"token": testToken,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, Config{})
	identity, err := client.VerifyIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.AccountID != "account-1" || identity.DeploymentID != "deploy-1" || identity.Version != SupportedVersion {
		t.Fatalf("unexpected identity: %#v", identity)
	}
	if identity.SiteID == "" || strings.Contains(identity.SiteID, server.URL) {
		t.Fatalf("site id must be a non-empty opaque binding: %q", identity.SiteID)
	}
	if got := strings.Join(paths, ","); got != "GET /api/version,GET /api/v1/auths/" {
		t.Fatalf("unexpected contract paths: %s", got)
	}

	otherURL, _ := newTestURL(server.URL + "/different-prefix")
	if deriveSiteID(otherURL, "deploy-1") == identity.SiteID {
		t.Fatal("site binding did not include the configured base URL")
	}
}

func TestVerifyIdentityRejectsVersionAndIdentityMismatch(t *testing.T) {
	tests := []struct {
		name            string
		version         string
		deploymentID    string
		accountID       string
		expectedSite    string
		expectedAccount string
	}{
		{name: "version", version: "0.11.4", deploymentID: "deploy", accountID: "account"},
		{name: "missing deployment", version: "0.11.3", accountID: "account"},
		{name: "account", version: "0.11.3", deploymentID: "deploy", accountID: "other", expectedAccount: "account"},
		{name: "site", version: "0.11.3", deploymentID: "deploy", accountID: "account", expectedSite: "sha256:wrong"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/version" {
					writeJSON(t, w, map[string]any{"version": test.version, "deployment_id": test.deploymentID})
					return
				}
				writeJSON(t, w, map[string]any{"id": test.accountID})
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, Config{
				ExpectedSiteID: test.expectedSite, ExpectedAccountID: test.expectedAccount,
			})
			if _, err := client.VerifyIdentity(context.Background()); err == nil {
				t.Fatal("mismatch was accepted")
			} else if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error leaked configured data: %v", err)
			}
		})
	}
}

func TestNewRejectsUnsupportedConfiguredVersion(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://owu.example.test", Token: "token", ExpectedVersion: "0.11.4"}); err == nil {
		t.Fatal("adapter accepted an unverified version contract")
	}
}

func TestClientCreateReadUpdateExactPathsAndReadback(t *testing.T) {
	var mu sync.Mutex
	var stored json.RawMessage
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("missing fixed bearer token on %s", r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/new":
			stored = requestChat(t, r.Body)
			stored = setChatField(t, stored, "id", "chat-1")
			writeChatResponse(t, w, "chat-1", "account-1", stored, 10)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/chats/chat-1":
			writeChatResponse(t, w, "chat-1", "account-1", stored, 11)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/chat-1":
			stored = requestChat(t, r.Body)
			writeChatResponse(t, w, "chat-1", "account-1", stored, 12)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, Config{ExpectedAccountID: "account-1"})
	desired, err := BuildSnapshot(testSource("Title", "question", "answer"), nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := client.Create(context.Background(), CreateRequest{OperationID: "op-create", Snapshot: desired})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.TargetID != "chat-1" || !MatchSnapshot(receipt.Snapshot, desired) {
		t.Fatalf("untrusted or semantically different create receipt: %#v", receipt)
	}
	readback, err := client.Read(context.Background(), "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if !Equivalent(receipt.Snapshot, readback) || !MatchSnapshot(readback, desired) {
		t.Fatal("create readback did not match the trusted receipt and desired graph")
	}

	updatedSource := testSource("New title", "changed question", "answer")
	updatedSource.Messages = append(updatedSource.Messages, domain.Message{
		ID: "u2", NodeID: "node-u2", Role: "user", Parts: []string{"more"}, Order: 2,
	})
	desiredUpdate, err := BuildSnapshot(updatedSource, &readback)
	if err != nil {
		t.Fatal(err)
	}
	updateReceipt, err := client.Update(context.Background(), "chat-1", UpdateRequest{
		OperationID: "op-update", Snapshot: desiredUpdate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !MatchSnapshot(updateReceipt.Snapshot, desiredUpdate) {
		t.Fatal("update response did not contain the complete desired graph")
	}
	finalRead, err := client.Read(context.Background(), "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if !MatchSnapshot(finalRead, desiredUpdate) || finalRead.Normalized.Title != "New title" || len(finalRead.Normalized.Messages) != 3 {
		t.Fatalf("unexpected update readback: %#v", finalRead.Normalized)
	}
	if got := strings.Join(calls, ","); got != "POST /api/v1/chats/new,GET /api/v1/chats/chat-1,POST /api/v1/chats/chat-1,GET /api/v1/chats/chat-1" {
		t.Fatalf("unexpected OWU calls: %s", got)
	}
}

func TestWriteFailuresHaveSafeOutcomeSemantics(t *testing.T) {
	desired, err := BuildSnapshot(testSource("Title", "q", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		status   int
		body     string
		unknown  bool
		maxBytes int64
		delay    time.Duration
	}{
		{name: "malformed success", status: 200, body: `{`, unknown: true},
		{name: "oversized success", status: 200, body: strings.Repeat("x", 128), maxBytes: 32, unknown: true},
		{name: "bad request may follow commit", status: 400, body: `{"detail":"` + testToken + `"}`, unknown: true},
		{name: "request timeout", status: 408, body: `{}`, unknown: true},
		{name: "rate limit", status: 429, body: `{}`, unknown: true},
		{name: "server error", status: 500, body: `{}`, unknown: true},
		{name: "unauthorized is definite", status: 401, body: `{"detail":"` + testToken + `"}`},
		{name: "client timeout", status: 200, body: `{}`, delay: 100 * time.Millisecond, unknown: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.delay > 0 {
					time.Sleep(test.delay)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			timeout := time.Second
			if test.delay > 0 {
				timeout = 10 * time.Millisecond
			}
			client := newTestClient(t, server.URL, Config{MaxResponseBytes: test.maxBytes, Timeout: timeout})
			receipt, err := client.Create(context.Background(), CreateRequest{OperationID: "op", Snapshot: desired})
			if err == nil || receipt.TargetID != "" {
				t.Fatalf("write failure returned a target receipt: %#v, %v", receipt, err)
			}
			if IsOutcomeUnknown(err) != test.unknown {
				t.Fatalf("unexpected outcome classification: %T %v", err, err)
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), test.body) {
				t.Fatalf("error leaked response/configuration data: %v", err)
			}
		})
	}
}

func TestRedirectsAreRejectedWithoutForwardingCredential(t *testing.T) {
	var reached bool
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		if r.Header.Get("Authorization") != "" {
			t.Error("credential was forwarded across redirect")
		}
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	client := newTestClient(t, redirect.URL, Config{})
	if _, err := client.VerifyIdentity(context.Background()); err == nil {
		t.Fatal("redirect was accepted")
	}
	if reached {
		t.Fatal("redirect destination was reached")
	}
}

func TestReadRejectsMalformedGraphAndDifferentIdentity(t *testing.T) {
	desired, err := BuildSnapshot(testSource("Title", "q", "a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeChatResponse(t, w, "other-chat", "other-account", desired.RawChat, 1)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, Config{ExpectedAccountID: "account-1"})
	if _, err := client.Read(context.Background(), "chat-1"); err == nil {
		t.Fatal("different target identity was accepted")
	}
	if _, err := client.Read(context.Background(), "../chats"); err == nil {
		t.Fatal("unsafe target id was accepted")
	}
}

func newTestClient(t *testing.T, baseURL string, overrides Config) *Client {
	t.Helper()
	config := overrides
	config.BaseURL = baseURL
	config.Token = testToken
	if config.ExpectedVersion == "" {
		config.ExpectedVersion = SupportedVersion
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func newTestURL(raw string) (*url.URL, error) { return url.Parse(raw) }

func requestChat(t *testing.T, body io.Reader) json.RawMessage {
	t.Helper()
	var request struct {
		Chat json.RawMessage `json:"chat"`
	}
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if len(request.Chat) == 0 {
		t.Fatal("request omitted chat")
	}
	return request.Chat
}

func setChatField(t *testing.T, raw json.RawMessage, key string, value any) json.RawMessage {
	t.Helper()
	var chat map[string]any
	if err := json.Unmarshal(raw, &chat); err != nil {
		t.Fatal(err)
	}
	chat[key] = value
	encoded, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func writeChatResponse(t *testing.T, w http.ResponseWriter, id, owner string, chat json.RawMessage, updated int64) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(chat, &doc); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, w, map[string]any{
		"id": id, "user_id": owner, "title": doc["title"], "chat": doc,
		"archived": false, "pinned": false, "folder_id": nil,
		"meta": map[string]any{"unknown": "preserve"}, "variables": map[string]any{"v": 1},
		"created_at": 1, "updated_at": updated, "context_usage": map[string]any{"volatile": updated},
	})
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func testSource(title, question, answer string) domain.SourceSnapshot {
	return domain.SourceSnapshot{
		SourceType: domain.SourceChatGPTShare,
		Identity:   domain.SourceIdentity{CandidateID: "source-1", Evidence: domain.EvidenceVerified},
		ShareID:    "share-1", Title: title, CurrentNodeID: "node-a1",
		Messages: []domain.Message{
			{ID: "u1", NodeID: "node-u1", Role: "user", Parts: []string{question}, ChildNodeIDs: []string{"node-a1"}, Order: 0},
			{ID: "a1", NodeID: "node-a1", Role: "assistant", Channel: "final", Parts: []string{answer}, ParentNodeID: "node-u1", Order: 1},
		},
	}
}

func TestOutcomeUnknownSupportsWrappedErrors(t *testing.T) {
	err := errors.Join(errors.New("outer"), &OutcomeUnknownError{Operation: "create"})
	if !IsOutcomeUnknown(err) {
		t.Fatal("wrapped outcome-unknown error was not recognized")
	}
}
