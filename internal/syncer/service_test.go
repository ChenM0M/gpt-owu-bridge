package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
	"github.com/ChenM0M/gpt-owu-bridge/internal/owu"
	"github.com/ChenM0M/gpt-owu-bridge/internal/storage"
)

func TestCreateUpdateReplayAndNoChangeReread(t *testing.T) {
	h := newHarness(t, Options{AllowSyntheticStableSourceIdentity: true})
	defer h.close()
	ctx := context.Background()

	first := testSource("Initial", "question", "answer")
	plan, err := h.service.Preview(ctx, h.installation.OwnerID, first, "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != domain.OperationSucceeded || created.BindingID == "" {
		t.Fatalf("create did not return a durable binding: %#v", created)
	}
	if got := h.remote.counts().creates; got != 1 {
		t.Fatalf("create calls=%d want=1", got)
	}
	replayed, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, false)
	if err != nil || replayed.ID != created.ID || h.remote.counts().creates != 1 {
		t.Fatalf("create replay was not idempotent: operation=%#v err=%v counts=%#v", replayed, err, h.remote.counts())
	}

	second := testSource("Updated", "edited question", "answer", "follow up")
	updatePlan, err := h.service.Preview(ctx, h.installation.OwnerID, second, created.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if updatePlan.Status != "ready" {
		t.Fatalf("update plan=%#v", updatePlan)
	}
	updated, err := h.service.Apply(ctx, h.installation.OwnerID, updatePlan.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != domain.OperationSucceeded || h.remote.counts().updates != 1 {
		t.Fatalf("update not verified: operation=%#v counts=%#v", updated, h.remote.counts())
	}

	noChangePlan, err := h.service.Preview(ctx, h.installation.OwnerID, second, created.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if noChangePlan.Status != "no_change" {
		t.Fatalf("no-change plan=%#v", noChangePlan)
	}
	// A native metadata edit is invisible in the normalized projection but is
	// conflict relevant. Apply must reread and reject it without a POST.
	h.remote.mutateChat(t, "target-1", func(chat map[string]json.RawMessage) {
		chat["native_extension"] = json.RawMessage(`{"changed":true}`)
	})
	operation, err := h.service.Apply(ctx, h.installation.OwnerID, noChangePlan.ID, true)
	if err == nil || operation.Status != domain.OperationFailed || operation.LastErrorCode != "target_conflict" {
		t.Fatalf("no-change target drift was accepted: operation=%#v err=%v", operation, err)
	}
	if got := h.remote.counts().updates; got != 1 {
		t.Fatalf("no-change drift caused a write: updates=%d", got)
	}
}

func TestCreatePreviewWithIgnoredAndDegradedDiagnosticsIsReady(t *testing.T) {
	h := newHarness(t, Options{})
	defer h.close()
	source := testSource("Search-backed answer", "question", "answer [Source](https://example.com)")
	source.Coverage.Diagnostics = []domain.ContentDiagnostic{
		{SourceID: "tool-1", Role: "tool", ContentType: "text", Disposition: "ignored", Detail: "tool result"},
		{SourceID: source.Messages[1].ID, Role: "assistant", ContentType: "citation", Disposition: "degraded", Detail: "Markdown link"},
	}
	source.BusinessHash, _ = source.ComputeHash()
	plan, err := h.service.Preview(context.Background(), h.installation.OwnerID, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "ready" || plan.ReadOnly || len(plan.AllowedActions) != 1 || plan.AllowedActions[0] != "apply_sync" {
		t.Fatalf("non-blocking diagnostics did not produce an applicable ready plan: %#v", plan)
	}
}

func TestCreateUnknownWithoutReceiptNeverRetriesAcrossRestart(t *testing.T) {
	h := newHarness(t, Options{})
	defer h.close()
	ctx := context.Background()
	h.remote.setMode(modeCreateUnknown)
	source := testSource("Unknown create", "question")
	plan, err := h.service.Preview(ctx, h.installation.OwnerID, source, "")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, true)
	if err == nil || operation.Status != domain.OperationNeedsReconciliation || operation.RemoteReceiptID != "" {
		t.Fatalf("unknown create state=%#v err=%v", operation, err)
	}
	if h.remote.counts().creates != 1 {
		t.Fatal("expected exactly one create request")
	}

	databasePath := h.databasePath
	h.store.Close()
	reopened, installation, err := storage.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	h.store = reopened
	h.installation = installation
	h.service = New(reopened, h.client, installation, Options{})
	readsBefore := h.remote.counts().reads
	recovered, recoverErr := h.service.Recover(ctx, installation.OwnerID, operation.ID)
	if recoverErr == nil || recovered.Status != domain.OperationNeedsReconciliation {
		t.Fatalf("receipt-less create recovery=%#v err=%v", recovered, recoverErr)
	}
	if h.remote.counts().reads != readsBefore {
		t.Fatal("receipt-less recovery read an untrusted target")
	}

	// A new plan and caller request for the same source returns the persisted
	// uncertain operation and never sends a second POST.
	fresh, err := h.service.Preview(ctx, installation.OwnerID, source, "")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := h.service.Apply(ctx, installation.OwnerID, fresh.ID, true)
	if err != nil || replayed.ID != operation.ID || h.remote.counts().creates != 1 {
		t.Fatalf("restart replay duplicated create: operation=%#v err=%v counts=%#v", replayed, err, h.remote.counts())
	}

	changed := testSource("Unknown create", "changed question")
	changedPlan, err := h.service.Preview(ctx, installation.OwnerID, changed, "")
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := h.service.Apply(ctx, installation.OwnerID, changedPlan.ID, true)
	if err == nil || conflict.ID != operation.ID || h.remote.counts().creates != 1 {
		t.Fatalf("changed payload reused creation identity: operation=%#v err=%v", conflict, err)
	}
}

func TestUnknownUpdateRecoveryOnlyReadsExpectedOrBaseline(t *testing.T) {
	h := newHarness(t, Options{AllowSyntheticStableSourceIdentity: true})
	defer h.close()
	ctx := context.Background()
	created := h.create(t, testSource("Title", "one"))

	h.remote.setMode(modeUpdateUnknownApplied)
	changed := testSource("Title", "one edited", "two")
	plan, err := h.service.Preview(ctx, h.installation.OwnerID, changed, created.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, true)
	if err == nil || operation.Status != domain.OperationNeedsReconciliation {
		t.Fatalf("unknown update=%#v err=%v", operation, err)
	}
	writes := h.remote.counts().updates
	h.remote.setMode(modeNormal)
	recovered, err := h.service.Recover(ctx, h.installation.OwnerID, operation.ID)
	if err != nil || recovered.Status != domain.OperationSucceeded {
		t.Fatalf("expected update was not recovered: operation=%#v err=%v", recovered, err)
	}
	if h.remote.counts().updates != writes {
		t.Fatal("recovery retried an update write")
	}

	// If the server initially exposes the baseline, the operation stays
	// unresolved because the old POST may still commit later.
	h.remote.setMode(modeUpdateUnknownDelayed)
	third := testSource("Title", "one edited", "two", "three")
	thirdPlan, err := h.service.Preview(ctx, h.installation.OwnerID, third, created.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	delayed, err := h.service.Apply(ctx, h.installation.OwnerID, thirdPlan.ID, true)
	if err == nil || delayed.Status != domain.OperationNeedsReconciliation {
		t.Fatalf("delayed update=%#v err=%v", delayed, err)
	}
	writes = h.remote.counts().updates
	stillUnknown, err := h.service.Recover(ctx, h.installation.OwnerID, delayed.ID)
	if err == nil || stillUnknown.Status != domain.OperationNeedsReconciliation {
		t.Fatalf("baseline read prematurely resolved delayed write: operation=%#v err=%v", stillUnknown, err)
	}
	if h.remote.counts().updates != writes {
		t.Fatal("baseline recovery retried the update")
	}
	h.remote.commitPending(t)
	h.remote.setMode(modeNormal)
	finally, err := h.service.Recover(ctx, h.installation.OwnerID, delayed.ID)
	if err != nil || finally.Status != domain.OperationSucceeded || h.remote.counts().updates != writes {
		t.Fatalf("late commit recovery failed or rewrote: operation=%#v err=%v", finally, err)
	}
}

func TestRemoteCreateThenReceiptPersistenceFailureDoesNotAdoptOrRetry(t *testing.T) {
	h := newHarness(t, Options{})
	defer h.close()
	ctx := context.Background()
	failing := &failReceiptRepository{Store: h.store, fail: true}
	h.service = New(failing, h.client, h.installation, Options{})
	source := testSource("Commit failure", "question")
	plan, err := h.service.Preview(ctx, h.installation.OwnerID, source, "")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, true)
	if err == nil || operation.Status != domain.OperationApplying || h.remote.counts().creates != 1 {
		t.Fatalf("injected receipt persistence failure=%#v err=%v", operation, err)
	}
	databasePath := h.databasePath
	h.store.Close()
	reopened, installation, err := storage.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	h.store, h.installation = reopened, installation
	h.service = New(reopened, h.client, installation, Options{})
	reads := h.remote.counts().reads
	recovered, recoverErr := h.service.Recover(ctx, installation.OwnerID, operation.ID)
	if recoverErr == nil || recovered.Status != domain.OperationNeedsReconciliation || h.remote.counts().reads != reads {
		t.Fatalf("unpersisted receipt was adopted: operation=%#v err=%v", recovered, recoverErr)
	}
	replayed, err := h.service.Apply(ctx, installation.OwnerID, plan.ID, true)
	if err != nil || replayed.ID != operation.ID || h.remote.counts().creates != 1 {
		t.Fatalf("create was retried after receipt persistence failure: operation=%#v err=%v", replayed, err)
	}
}

func TestConcurrentApplySendsOneCreate(t *testing.T) {
	h := newHarness(t, Options{})
	defer h.close()
	ctx := context.Background()
	plan, err := h.service.Preview(ctx, h.installation.OwnerID, testSource("Concurrent", "one"), "")
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		op  domain.Operation
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			op, applyErr := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, true)
			results <- result{op: op, err: applyErr}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.op.ID == "" || first.op.ID != second.op.ID {
		t.Fatalf("concurrent results: first=%#v second=%#v", first, second)
	}
	if h.remote.counts().creates != 1 {
		t.Fatalf("concurrent apply sent %d creates", h.remote.counts().creates)
	}
	status, err := h.service.Status(ctx, h.installation.OwnerID, first.op.ID)
	if err != nil || status.Status != domain.OperationSucceeded {
		t.Fatalf("final concurrent status=%#v err=%v", status, err)
	}
}

func TestConfirmationExpiryIdentityAndSourceGuards(t *testing.T) {
	now := time.Now().UTC()
	h := newHarness(t, Options{Now: func() time.Time { return now }, PlanTTL: time.Minute})
	defer h.close()
	ctx := context.Background()
	plan, err := h.service.Preview(ctx, h.installation.OwnerID, testSource("Guards", "one"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, false); resultCode(err) != "confirmation_required" {
		t.Fatalf("confirmation guard error=%v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := h.service.Apply(ctx, h.installation.OwnerID, plan.ID, true); resultCode(err) != "plan_expired" {
		t.Fatalf("expiry guard error=%v", err)
	}
	if _, err := h.service.Preview(ctx, "other-owner", testSource("Guards", "one"), ""); resultCode(err) != "unauthorized_owner" {
		t.Fatalf("owner guard error=%v", err)
	}
	h.remote.setAccount("other-account")
	if _, err := h.service.Preview(ctx, h.installation.OwnerID, testSource("Guards", "one"), ""); resultCode(err) != "target_identity_mismatch" {
		t.Fatalf("target identity guard error=%v", err)
	}
}

type harness struct {
	t            *testing.T
	remote       *mockOWU
	server       *httptest.Server
	client       *owu.Client
	store        *storage.Store
	installation storage.Installation
	service      *Service
	databasePath string
}

func newHarness(t *testing.T, options Options) *harness {
	t.Helper()
	remote := newMockOWU()
	server := httptest.NewServer(remote)
	client, err := owu.New(owu.Config{BaseURL: server.URL, Token: "test-token", ExpectedVersion: owu.SupportedVersion})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	identity, err := client.VerifyIdentity(context.Background())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "private-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		server.Close()
		t.Fatal(err)
	}
	databasePath := filepath.Join(dataDir, storage.DefaultDatabaseFilename)
	store, installation, err := storage.Initialize(context.Background(), databasePath, storage.TargetIdentity{
		OWUSiteID: identity.SiteID, OWUAccountID: identity.AccountID,
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return &harness{
		t: t, remote: remote, server: server, client: client, store: store,
		installation: installation, service: New(store, client, installation, options), databasePath: databasePath,
	}
}

func (h *harness) close() {
	if h.store != nil {
		_ = h.store.Close()
	}
	if h.server != nil {
		h.server.Close()
	}
}

func (h *harness) create(t *testing.T, source domain.SourceSnapshot) domain.Operation {
	t.Helper()
	plan, err := h.service.Preview(context.Background(), h.installation.OwnerID, source, "")
	if err != nil {
		t.Fatal(err)
	}
	operation, err := h.service.Apply(context.Background(), h.installation.OwnerID, plan.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

type failReceiptRepository struct {
	*storage.Store
	mu   sync.Mutex
	fail bool
}

func (r *failReceiptRepository) TransitionOperation(ctx context.Context, id string, expected []domain.OperationStatus, next domain.OperationStatus, patch storage.TransitionPatch) (storage.OperationRecord, error) {
	r.mu.Lock()
	shouldFail := r.fail && next == domain.OperationVerifying
	if shouldFail {
		r.fail = false
	}
	r.mu.Unlock()
	if shouldFail {
		record, _ := r.Store.GetOperation(ctx, id)
		return record, errors.New("injected receipt persistence failure")
	}
	return r.Store.TransitionOperation(ctx, id, expected, next, patch)
}

type remoteMode int

const (
	modeNormal remoteMode = iota
	modeCreateUnknown
	modeUpdateUnknownApplied
	modeUpdateUnknownDelayed
)

type remoteCounts struct{ creates, updates, reads int }

type mockOWU struct {
	mu           sync.Mutex
	deploymentID string
	accountID    string
	mode         remoteMode
	chats        map[string]json.RawMessage
	pending      json.RawMessage
	createCount  int
	updateCount  int
	readCount    int
}

func newMockOWU() *mockOWU {
	return &mockOWU{deploymentID: "test-deployment", accountID: "test-account", chats: make(map[string]json.RawMessage)}
}

func (m *mockOWU) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/version":
		writeJSON(w, map[string]any{"version": owu.SupportedVersion, "deployment_id": m.deploymentID})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auths/":
		writeJSON(w, map[string]any{"id": m.accountID, "email": "owner@example.invalid", "name": "Owner", "role": "user"})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/new":
		m.createCount++
		chat, err := decodeChatRequest(r)
		if err != nil {
			http.Error(w, "bad", http.StatusUnprocessableEntity)
			return
		}
		m.chats["target-1"] = chat
		if m.mode == modeCreateUnknown {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"ambiguous"}`))
			return
		}
		writeJSON(w, m.envelope("target-1", chat))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/chats/target-1":
		m.readCount++
		chat, ok := m.chats["target-1"]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, m.envelope("target-1", chat))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/target-1":
		m.updateCount++
		chat, err := decodeChatRequest(r)
		if err != nil {
			http.Error(w, "bad", http.StatusUnprocessableEntity)
			return
		}
		switch m.mode {
		case modeUpdateUnknownApplied:
			m.chats["target-1"] = chat
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"ambiguous"}`))
		case modeUpdateUnknownDelayed:
			m.pending = chat
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"ambiguous"}`))
		default:
			m.chats["target-1"] = chat
			writeJSON(w, m.envelope("target-1", chat))
		}
	default:
		http.NotFound(w, r)
	}
}

func (m *mockOWU) envelope(id string, chat json.RawMessage) map[string]any {
	var body map[string]any
	_ = json.Unmarshal(chat, &body)
	return map[string]any{
		"id": id, "user_id": m.accountID, "title": body["title"], "chat": body,
		"archived": false, "pinned": false, "folder_id": nil, "updated_at": int64(100),
		"meta": map[string]any{"stable": "preserved"},
	}
}

func (m *mockOWU) counts() remoteCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return remoteCounts{m.createCount, m.updateCount, m.readCount}
}

func (m *mockOWU) setMode(mode remoteMode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = mode
}

func (m *mockOWU) setAccount(account string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountID = account
}

func (m *mockOWU) mutateChat(t *testing.T, id string, mutate func(map[string]json.RawMessage)) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var chat map[string]json.RawMessage
	if err := json.Unmarshal(m.chats[id], &chat); err != nil {
		t.Fatal(err)
	}
	mutate(chat)
	raw, err := json.Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	m.chats[id] = raw
}

func (m *mockOWU) commitPending(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		t.Fatal("no pending update")
	}
	m.chats["target-1"] = append(json.RawMessage(nil), m.pending...)
	m.pending = nil
}

func decodeChatRequest(r *http.Request) (json.RawMessage, error) {
	defer r.Body.Close()
	var request struct {
		Chat json.RawMessage `json:"chat"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil || len(request.Chat) == 0 {
		return nil, errors.New("invalid chat request")
	}
	return append(json.RawMessage(nil), request.Chat...), nil
}

func writeJSON(w http.ResponseWriter, value any) {
	_ = json.NewEncoder(w).Encode(value)
}

func testSource(title string, texts ...string) domain.SourceSnapshot {
	roles := []string{"user", "assistant"}
	messages := make([]domain.Message, len(texts))
	for index, text := range texts {
		nodeID := fmt.Sprintf("node-%d", index+1)
		messages[index] = domain.Message{
			ID: fmt.Sprintf("message-%d", index+1), NodeID: nodeID,
			Role: roles[index%len(roles)], Parts: []string{text}, Order: index,
		}
		if index > 0 {
			messages[index].ParentNodeID = messages[index-1].NodeID
			messages[index-1].ChildNodeIDs = []string{nodeID}
		}
	}
	snapshot := domain.SourceSnapshot{
		SourceType: domain.SourceChatGPTShare,
		Identity:   domain.SourceIdentity{CandidateID: "source-stable", Evidence: domain.EvidenceCandidate, Basis: "synthetic test"},
		ShareID:    "share-stable", Title: title, Messages: messages,
		CurrentNodeID: messages[len(messages)-1].NodeID, FetchedAt: time.Now().UTC(),
		ParserVersion: domain.ParserVersion,
		Coverage:      domain.Coverage{Status: "supported_path_complete", SelectedMessages: len(messages)},
	}
	snapshot.BusinessHash, _ = snapshot.ComputeHash()
	return snapshot
}

func resultCode(err error) string {
	var result *domain.ResultError
	if errors.As(err, &result) {
		return result.Code
	}
	return ""
}
