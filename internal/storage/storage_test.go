package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

func TestInitializeOpenIdentityPermissionsAndExclusiveLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(privateTempDir(t), DefaultDatabaseFilename)
	target := TargetIdentity{OWUSiteID: "site-a", OWUAccountID: "account-a"}
	store, installation, err := Initialize(ctx, path, target)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if installation.ID == "" || installation.OwnerID == "" || installation.ID == installation.OwnerID {
		t.Fatalf("installation IDs were not generated independently: %+v", installation)
	}
	if err := store.VerifyTargetIdentity(target); err != nil {
		t.Fatalf("fixed identity should match: %v", err)
	}
	if err := store.VerifyTargetIdentity(TargetIdentity{OWUSiteID: "site-a", OWUAccountID: "other"}); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("identity mismatch = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %#o, want 0600", got)
	}
	if _, _, err := Initialize(ctx, path, target); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second initialize = %v", err)
	}
	if other, _, err := Open(ctx, path); !errors.Is(err, ErrDatabaseInUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("concurrent open = %v, want database in use", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestStorageLockHelperProcess$")
	command.Env = append(os.Environ(), "GPT_OWU_GATE_LOCK_TEST_PATH="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-process exclusive lock was not enforced: %v\n%s", err, output)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, got, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got != installation {
		t.Fatalf("installation changed across restart:\n got %+v\nwant %+v", got, installation)
	}
}

func TestStorageLockHelperProcess(t *testing.T) {
	path := os.Getenv("GPT_OWU_GATE_LOCK_TEST_PATH")
	if path == "" {
		t.Skip("helper process only")
	}
	store, _, err := Open(context.Background(), path)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, ErrDatabaseInUse) {
		t.Fatalf("concurrent process open = %v, want database in use", err)
	}
}

func TestInitializeRefusesOrphanedSidecar(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, DefaultDatabaseFilename)
	if err := os.WriteFile(path+"-wal", []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := Initialize(context.Background(), path,
		TargetIdentity{OWUSiteID: "site", OWUAccountID: "account"})
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("initialize with orphaned WAL = %v", err)
	}
}

func TestSourceSnapshotDeduplicatesFetchObservations(t *testing.T) {
	store := newTestStore(t)
	first := testSource(t, "share-1", "title", time.Unix(10, 0))
	if err := store.PutSourceSnapshot(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.FetchedAt = time.Unix(20, 0)
	if err := store.PutSourceSnapshot(context.Background(), second); err != nil {
		t.Fatalf("same business snapshot at a later fetch should deduplicate: %v", err)
	}
	stored, err := store.GetSourceSnapshot(context.Background(), first.BusinessHash)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("first immutable observation was replaced: %v", stored.FetchedAt)
	}
}

func TestCreateIntentIdempotencyAliasClaimAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	dir := privateTempDir(t)
	path := filepath.Join(dir, DefaultDatabaseFilename)
	store, _, err := Initialize(ctx, path, TargetIdentity{OWUSiteID: "site", OWUAccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	source := testSource(t, "share-1", "first", time.Unix(1, 0))
	if err := store.PutSourceSnapshot(ctx, source); err != nil {
		t.Fatal(err)
	}
	plan := testPlan("plan-1", source, "desired", true, 0, "")
	if err := store.SavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{OperationID: "op-1", IdempotencyKey: "create:share-1",
		PlanID: "plan-1", PayloadHash: "semantic-a", SourceSnapshotHash: source.BusinessHash}
	if _, _, err := store.PrepareCreate(ctx, request); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed create = %v", err)
	}
	if _, err := store.ConfirmPlan(ctx, "plan-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	op, created, err := store.PrepareCreate(ctx, request)
	if err != nil || !created || op.Operation.Status != domain.OperationPrepared {
		t.Fatalf("prepare create = %+v, %v, %v", op, created, err)
	}

	// A new preview with a different plan ID replays the same semantic request.
	plan2 := testPlan("plan-2", source, "desired", false, 0, "")
	if err := store.SavePlan(ctx, plan2); err != nil {
		t.Fatal(err)
	}
	replay := request
	replay.OperationID = "op-2"
	replay.PlanID = "plan-2"
	got, created, err := store.PrepareCreate(ctx, replay)
	if err != nil || created || got.Operation.ID != "op-1" {
		t.Fatalf("idempotent replay = %+v, %v, %v", got, created, err)
	}
	replay.PayloadHash = "semantic-b"
	if _, _, err := store.PrepareCreate(ctx, replay); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload with same idempotency key = %v", err)
	}

	if _, err := store.TransitionOperation(ctx, "op-1", []domain.OperationStatus{domain.OperationPrepared},
		domain.OperationApplying, TransitionPatch{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.GetOperation(ctx, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Operation.Status != domain.OperationNeedsReconciliation || !recovered.Operation.NeedsReadback {
		t.Fatalf("interrupted operation was not made uncertain: %+v", recovered)
	}

	changed := testSource(t, "share-1", "changed", time.Unix(2, 0))
	if err := reopened.PutSourceSnapshot(ctx, changed); err != nil {
		t.Fatal(err)
	}
	changedPlan := testPlan("plan-3", changed, "changed", false, 0, "")
	if err := reopened.SavePlan(ctx, changedPlan); err != nil {
		t.Fatal(err)
	}
	_, _, err = reopened.PrepareCreate(ctx, CreateRequest{
		OperationID: "op-3", IdempotencyKey: "create:share-1:changed",
		PlanID: "plan-3", PayloadHash: "semantic-changed", SourceSnapshotHash: changed.BusinessHash,
	})
	if !errors.Is(err, ErrSourceAlreadyClaimed) {
		t.Fatalf("same source alias after unknown create = %v", err)
	}
}

func TestVerifiedCreateIsOnlyPathToAllowlist(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	source := testSource(t, "share-verified", "source", time.Now())
	if err := store.PutSourceSnapshot(ctx, source); err != nil {
		t.Fatal(err)
	}
	plan := testPlan("plan-create", source, "target", false, 0, "")
	if err := store.SavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	_, _, err := store.PrepareCreate(ctx, CreateRequest{OperationID: "op-create",
		IdempotencyKey: "create:verified", PlanID: plan.Plan.ID,
		PayloadHash: "semantic-create", SourceSnapshotHash: source.BusinessHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionOperation(ctx, "op-create", []domain.OperationStatus{domain.OperationPrepared},
		domain.OperationApplying, TransitionPatch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTrustedCreateTarget(ctx, "op-create"); !errors.Is(err, ErrUntrustedTarget) {
		t.Fatalf("create target trusted before receipt = %v", err)
	}
	needsReadback := true
	if _, err := store.TransitionOperation(ctx, "op-create", []domain.OperationStatus{domain.OperationApplying},
		domain.OperationVerifying, TransitionPatch{TargetChatID: "chat-1", RemoteReceiptID: "chat-1", NeedsReadback: &needsReadback}); err != nil {
		t.Fatal(err)
	}
	trustedTarget, err := store.GetTrustedCreateTarget(ctx, "op-create")
	if err != nil || trustedTarget != "chat-1" {
		t.Fatalf("trusted create target = %q, %v", trustedTarget, err)
	}
	if _, err := store.GetBinding(ctx, "forged-binding"); !errors.Is(err, ErrUntrustedTarget) {
		t.Fatalf("arbitrary binding lookup = %v", err)
	}
	target := testTarget("target", "tm-1")
	binding, err := store.FinalizeVerifiedCreate(ctx, VerifiedCreate{
		OperationID: "op-create", BindingID: "binding-1", TargetChatID: "chat-1",
		RemoteReceiptID: "chat-1", SourceIdentity: source.Identity,
		MessageMappings: []domain.MessageMapping{{SourceMessageID: "m-1", TargetMessageID: "tm-1"}},
		TitlePolicy:     domain.TitleFollowSource, VerifiedTarget: target, VerifiedConflictHash: "conflict-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding.InstallationID != store.Installation().ID || binding.PrincipalID != store.Installation().OwnerID ||
		binding.OWUSiteID != "site" || binding.OWUAccountID != "account" {
		t.Fatalf("binding did not inherit fixed identity: %+v", binding)
	}
	state, err := store.GetBindingState(ctx, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.TargetBaseline.ConflictHash != "conflict-1" || state.Binding.TargetChatID != "chat-1" ||
		state.SourceSnapshot.BusinessHash != source.BusinessHash {
		t.Fatalf("unexpected binding state: %+v", state)
	}
	operation, err := store.GetOperation(ctx, "op-create")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Operation.Status != domain.OperationSucceeded || operation.Operation.NeedsReadback {
		t.Fatalf("verified create not completed: %+v", operation)
	}
	if operation.Operation.BindingID != binding.ID {
		t.Fatalf("completed create operation lost binding ID: %+v", operation)
	}
	if _, err := store.GetTrustedCreateTarget(ctx, "op-create"); !errors.Is(err, ErrUntrustedTarget) {
		t.Fatalf("pre-binding authorization remained available after finalization: %v", err)
	}

	updatedSource := testSource(t, "share-verified", "updated source", time.Now().Add(time.Second))
	if err := store.PutSourceSnapshot(ctx, updatedSource); err != nil {
		t.Fatal(err)
	}
	updatePlan := testPlan("plan-update", updatedSource, "updated target", false, binding.Version, binding.ID)
	updatePlan.PreviousSnapshotHash = source.BusinessHash
	updatePlan.ExpectedTargetHash = state.TargetBaseline.SnapshotHash
	updatePlan.ExpectedTargetConflictHash = state.TargetBaseline.ConflictHash
	if err := store.SavePlan(ctx, updatePlan); err != nil {
		t.Fatal(err)
	}
	_, created, err := store.PrepareBindingOperation(ctx, PrepareOperationRequest{
		OperationID: "op-update", IdempotencyKey: "update:binding-1:v1", Kind: OperationUpdate,
		BindingID: binding.ID, PlanID: updatePlan.Plan.ID, PayloadHash: "semantic-update",
	})
	if err != nil || !created {
		t.Fatalf("prepare update = %v, created=%v", err, created)
	}
	if _, err := store.TransitionOperation(ctx, "op-update", []domain.OperationStatus{domain.OperationPrepared},
		domain.OperationApplying, TransitionPatch{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionOperation(ctx, "op-update", []domain.OperationStatus{domain.OperationApplying},
		domain.OperationVerifying, TransitionPatch{RemoteReceiptID: "op-update", NeedsReadback: &needsReadback}); err != nil {
		t.Fatal(err)
	}
	updatedTarget := testTarget("updated target", "tm-1")
	if err := store.CompleteVerifiedUpdate(ctx, "op-update", TargetBaseline{
		BindingID: binding.ID, Snapshot: updatedTarget, ConflictHash: "conflict-2",
	}, binding.Version); err != nil {
		t.Fatal(err)
	}
	updatedState, err := store.GetBindingState(ctx, binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedState.Binding.Version != 2 || updatedState.TargetBaseline.ConflictHash != "conflict-2" ||
		updatedState.SourceSnapshot.BusinessHash != updatedSource.BusinessHash {
		t.Fatalf("verified update did not advance baseline atomically: %+v", updatedState)
	}

	nowNS := time.Now().UnixNano()
	_, err = store.conn.ExecContext(ctx, `
		INSERT INTO operations(
			operation_id, owner_id, idempotency_key, kind, binding_id, lock_key,
			plan_id, payload_hash, status, attempt_count, needs_readback,
			last_error_code, remote_receipt_id, target_chat_id, created_at_ns, updated_at_ns
		) VALUES ('forged-op', ?, 'forged-key', 'create', '', 'create:forged',
		          'plan-create', 'forged-payload', 'verifying', 1, 1,
		          '', 'forged-chat', 'forged-chat', ?, ?)`,
		store.Installation().OwnerID, nowNS, nowNS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTrustedCreateTarget(ctx, "forged-op"); !errors.Is(err, ErrUntrustedTarget) {
		t.Fatalf("operation row without creation intent authorized a GET: %v", err)
	}
	if _, err := store.conn.ExecContext(ctx,
		"UPDATE operations SET status = 'failed' WHERE operation_id = 'op-create'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBinding(ctx, binding.ID); !errors.Is(err, ErrUntrustedTarget) {
		t.Fatalf("broken creation evidence still authorized binding: %v", err)
	}
}

func TestCorruptionPermissionsIdentityAndUnknownMigrationFailClosed(t *testing.T) {
	t.Run("garbage", func(t *testing.T) {
		path := filepath.Join(privateTempDir(t), DefaultDatabaseFilename)
		if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Open(context.Background(), path)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("garbage database = %v", err)
		}
	})
	t.Run("permissions", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(privateTempDir(t), DefaultDatabaseFilename)
		store, _, err := Initialize(ctx, path, TargetIdentity{OWUSiteID: "site", OWUAccountID: "account"})
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err = Open(ctx, path)
		if !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("broad database permissions = %v", err)
		}
	})
	t.Run("unknown migration", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(privateTempDir(t), DefaultDatabaseFilename)
		store, _, err := Initialize(ctx, path, TargetIdentity{OWUSiteID: "site", OWUAccountID: "account"})
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = raw.Exec("INSERT INTO schema_migrations(version, name, applied_at) VALUES (999, 'future.sql', 'now')")
		_ = raw.Close()
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = Open(ctx, path)
		if err == nil {
			t.Fatal("future migration was accepted")
		}
	})
	t.Run("incomplete identity", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(privateTempDir(t), DefaultDatabaseFilename)
		store, _, err := Initialize(ctx, path, TargetIdentity{OWUSiteID: "site", OWUAccountID: "account"})
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = raw.Exec("UPDATE installations SET owu_account_id = '' WHERE singleton = 1")
		_ = raw.Close()
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = Open(ctx, path)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("incomplete identity = %v", err)
		}
	})
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, _, err := Initialize(context.Background(), filepath.Join(privateTempDir(t), DefaultDatabaseFilename),
		TargetIdentity{OWUSiteID: "site", OWUAccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testSource(t *testing.T, shareID, title string, fetched time.Time) domain.SourceSnapshot {
	t.Helper()
	snapshot := domain.SourceSnapshot{
		SourceType: domain.SourceChatGPTShare,
		Identity: domain.SourceIdentity{CandidateID: "candidate-" + shareID,
			Evidence: domain.EvidenceCandidate, Basis: "test"},
		ShareID: shareID, Title: title,
		Messages:      []domain.Message{{ID: "m-1", NodeID: "n-1", Role: "user", Parts: []string{"hello"}}},
		CurrentNodeID: "n-1", FetchedAt: fetched.UTC(), ParserVersion: domain.ParserVersion,
		Coverage: domain.Coverage{Status: "complete", SelectedMessages: 1},
	}
	hash, err := snapshot.ComputeHash()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.BusinessHash = hash
	return snapshot
}

func testPlan(id string, source domain.SourceSnapshot, title string, confirmation bool,
	bindingVersion uint64, bindingID string,
) StoredPlan {
	desired := testTarget(title, "tm-1")
	raw, _ := json.Marshal(map[string]any{"title": title, "messages": []string{"tm-1"}})
	return StoredPlan{
		Plan: domain.SyncPlan{ID: id, Status: "ready", SourceSnapshotHash: source.BusinessHash,
			BindingVersion: bindingVersion, ParserVersion: domain.ParserVersion,
			PolicyVersion: domain.PolicyVersion, ExpiresAt: time.Now().Add(time.Hour)},
		BindingID: bindingID, SourceSnapshotHash: source.BusinessHash,
		DesiredTarget: desired, DesiredConflictHash: "desired-conflict", DesiredRawChat: raw,
		ConfirmationRequired: confirmation,
	}
}

func testTarget(title, targetID string) domain.TargetSnapshot {
	return domain.TargetSnapshot{
		Title: title,
		Messages: []domain.TargetMessage{{ID: targetID, SourceID: "m-1", Role: "user",
			Parts: []string{"hello"}, Managed: true}},
		CurrentMessageID: targetID,
	}
}
