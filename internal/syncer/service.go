// Package syncer coordinates durable single-shot synchronization.  It is the
// only layer allowed to turn a persisted plan into Open WebUI writes.
package syncer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/diff"
	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
	"github.com/ChenM0M/gpt-owu-bridge/internal/owu"
	"github.com/ChenM0M/gpt-owu-bridge/internal/storage"
)

const defaultPlanTTL = 15 * time.Minute

// TargetClient deliberately has no list, search, adopt, or arbitrary request
// operation. A read can only use a target id obtained from durable local state.
type TargetClient interface {
	VerifyIdentity(context.Context) (owu.Identity, error)
	Create(context.Context, owu.CreateRequest) (owu.CreateReceipt, error)
	Read(context.Context, string) (owu.Snapshot, error)
	Update(context.Context, string, owu.UpdateRequest) (owu.UpdateReceipt, error)
}

// Repository is the narrow durable contract needed by the orchestrator.
// Implementations must make PrepareCreate persist the creation intent before
// returning and enforce the allow-list in PrepareBindingOperation.
type Repository interface {
	PutSourceSnapshot(context.Context, domain.SourceSnapshot) error
	GetSourceSnapshot(context.Context, string) (domain.SourceSnapshot, error)
	SavePlan(context.Context, storage.StoredPlan) error
	GetPlan(context.Context, string) (storage.StoredPlan, error)
	ConfirmPlan(context.Context, string, time.Time) (storage.StoredPlan, error)
	PrepareCreate(context.Context, storage.CreateRequest) (storage.OperationRecord, bool, error)
	PrepareBindingOperation(context.Context, storage.PrepareOperationRequest) (storage.OperationRecord, bool, error)
	TransitionOperation(context.Context, string, []domain.OperationStatus, domain.OperationStatus, storage.TransitionPatch) (storage.OperationRecord, error)
	FinalizeVerifiedCreate(context.Context, storage.VerifiedCreate) (domain.Binding, error)
	CompleteVerifiedUpdate(context.Context, string, storage.TargetBaseline, uint64) error
	GetBindingState(context.Context, string) (storage.BindingState, error)
	GetOperation(context.Context, string) (storage.OperationRecord, error)
	GetOperationByIdempotencyKey(context.Context, string) (storage.OperationRecord, error)
	GetTrustedCreateTarget(context.Context, string) (string, error)
	RecoverInterrupted(context.Context) ([]storage.OperationRecord, error)
}

type Options struct {
	Now     func() time.Time
	PlanTTL time.Duration

	// AllowSyntheticStableSourceIdentity is only for deterministic local tests.
	// Production callers must leave it false until real ChatGPT source identity
	// stability has been independently verified.
	AllowSyntheticStableSourceIdentity bool
}

type Service struct {
	store        Repository
	target       TargetClient
	installation storage.Installation
	now          func() time.Time
	planTTL      time.Duration
	stableIDs    bool
}

func New(store Repository, target TargetClient, installation storage.Installation, options Options) *Service {
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	ttl := options.PlanTTL
	if ttl <= 0 {
		ttl = defaultPlanTTL
	}
	return &Service{
		store: store, target: target, installation: installation, now: now,
		planTTL: ttl, stableIDs: options.AllowSyntheticStableSourceIdentity,
	}
}

// Preview verifies the fixed downstream identity before any target read,
// persists the immutable source and plan, and never accepts a target id from a
// caller. An empty binding id means a create plan.
func (s *Service) Preview(ctx context.Context, principalID string, snapshot domain.SourceSnapshot, bindingID string) (domain.SyncPlan, error) {
	if err := s.authorize(principalID); err != nil {
		return domain.SyncPlan{}, err
	}
	if err := s.verifyDownstream(ctx); err != nil {
		return domain.SyncPlan{}, err
	}
	if err := normalizeSource(&snapshot); err != nil {
		return domain.SyncPlan{}, resultError("invalid_source_snapshot", "the source snapshot is internally inconsistent", "refresh and parse the share again", false, "")
	}
	now := s.now().UTC()

	if bindingID == "" {
		preview, err := diff.Preview(nil, snapshot, nil, nil, diff.Options{Now: now})
		if err != nil {
			return domain.SyncPlan{}, err
		}
		desired, err := owu.BuildSnapshot(snapshot, nil)
		if err != nil {
			return domain.SyncPlan{}, fmt.Errorf("build target snapshot: %w", err)
		}
		plan := makePersistedPlan(preview, now, s.planTTL)
		if plan.Status == "needs_binding_confirmation" {
			plan.Status = "ready"
			plan.ReadOnly = false
			plan.AllowedActions = []string{"apply_sync"}
			plan.NextAction = "review the source range, then apply this exact plan with explicit confirmation"
		}
		stored, err := makeStoredPlan(plan, "", snapshot, nil, nil, desired, now, true)
		if err != nil {
			return domain.SyncPlan{}, err
		}
		stored.OwnerID = s.installation.OwnerID
		if err := s.persistPlan(ctx, snapshot, stored); err != nil {
			return domain.SyncPlan{}, err
		}
		return plan, nil
	}

	state, err := s.store.GetBindingState(ctx, bindingID)
	if err != nil {
		return domain.SyncPlan{}, mapStorageError(err, "load binding")
	}
	if err := s.validateBinding(state.Binding); err != nil {
		return domain.SyncPlan{}, err
	}
	actual, err := s.target.Read(ctx, state.Binding.TargetChatID)
	if err != nil {
		return domain.SyncPlan{}, resultError("target_unavailable", "the managed target could not be read", "retry preview after Open WebUI is available", true, "")
	}
	if actual.ChatID != state.Binding.TargetChatID || actual.OwnerID != s.installation.OWUAccountID {
		return domain.SyncPlan{}, resultError("target_identity_mismatch", "the target belongs to a different Open WebUI account", "restore the configured account identity", false, "")
	}

	stable := s.stableIDs || verifiedSameSource(state.SourceSnapshot.Identity, snapshot.Identity)
	baselineForDiff := state.TargetBaseline.Snapshot
	actualForDiff := actual.Normalized
	baselineForDiff.OfflineSimulation = true
	actualForDiff.OfflineSimulation = true
	preview, err := diff.Preview(&state.SourceSnapshot, snapshot, &baselineForDiff, &actualForDiff, diff.Options{
		Now: now, TitlePolicy: state.Binding.TitlePolicy, BindingVersion: state.Binding.Version, StableSourceIDs: stable,
	})
	if err != nil {
		return domain.SyncPlan{}, err
	}
	plan := makePersistedPlan(preview, now, s.planTTL)
	if !matchesBaseline(actual, state.TargetBaseline) {
		plan.Diff.TargetConflicts = append(plan.Diff.TargetConflicts, domain.MessageChange{
			Kind: "target_state_changed", Detail: "the complete target document differs from the last verified baseline",
		})
		plan.Status = "conflict"
		plan.ReadOnly = true
		plan.AllowedActions = nil
		plan.NextAction = "resolve the target change before creating another sync plan"
	}
	actualHash, err := actual.Normalized.ComputeHash()
	if err != nil {
		return domain.SyncPlan{}, err
	}
	plan.TargetBaselineHash = actualHash

	desired := actual
	if plan.Status == "ready" || plan.Status == "no_change" {
		desired, err = owu.BuildSnapshot(snapshot, &actual)
		if err != nil {
			return domain.SyncPlan{}, fmt.Errorf("build target update: %w", err)
		}
		plan.ReadOnly = false
		plan.AllowedActions = []string{"apply_sync"}
		if plan.Status == "ready" {
			plan.NextAction = "apply this exact plan with explicit confirmation"
		} else {
			plan.NextAction = "apply to record a verified no-change operation, or take no action"
		}
	}
	stored, err := makeStoredPlan(plan, bindingID, snapshot, &state.SourceSnapshot, &actual, desired, now, true)
	if err != nil {
		return domain.SyncPlan{}, err
	}
	stored.OwnerID = s.installation.OwnerID
	if err := s.persistPlan(ctx, snapshot, stored); err != nil {
		return domain.SyncPlan{}, err
	}
	return plan, nil
}

func (s *Service) persistPlan(ctx context.Context, snapshot domain.SourceSnapshot, plan storage.StoredPlan) error {
	if err := s.store.PutSourceSnapshot(ctx, snapshot); err != nil {
		return mapStorageError(err, "persist source snapshot")
	}
	if err := s.store.SavePlan(ctx, plan); err != nil {
		return mapStorageError(err, "persist sync plan")
	}
	return nil
}

// Apply binds confirmation to the persisted immutable plan before creating an
// operation. Replays return the already-persisted operation state.
func (s *Service) Apply(ctx context.Context, principalID, planID string, confirmed bool) (domain.Operation, error) {
	if err := s.authorize(principalID); err != nil {
		return domain.Operation{}, err
	}
	stored, err := s.store.GetPlan(ctx, planID)
	if err != nil {
		return domain.Operation{}, mapStorageError(err, "load sync plan")
	}
	if err := s.validatePlanOwnership(stored); err != nil {
		return domain.Operation{}, err
	}
	idempotencyKey, semanticPayload, err := s.operationIdentity(ctx, stored)
	if err != nil {
		return domain.Operation{}, err
	}
	if existing, lookupErr := s.store.GetOperationByIdempotencyKey(ctx, idempotencyKey); lookupErr == nil {
		if existing.PayloadHash != semanticPayload {
			return existing.Operation, mapStorageError(storage.ErrIdempotencyConflict, "replay operation")
		}
		return existing.Operation, nil
	} else if !errors.Is(lookupErr, storage.ErrNotFound) {
		return domain.Operation{}, mapStorageError(lookupErr, "look up operation replay")
	}
	if err := s.validatePlan(stored); err != nil {
		return domain.Operation{}, err
	}
	if stored.ConfirmationRequired && stored.ConfirmedAt == nil {
		if !confirmed {
			return domain.Operation{}, resultError("confirmation_required", "this immutable plan has not been confirmed", "repeat apply with explicit confirmation after reviewing the plan", false, "")
		}
		stored, err = s.store.ConfirmPlan(ctx, planID, s.now().UTC())
		if err != nil {
			return domain.Operation{}, mapStorageError(err, "confirm sync plan")
		}
	}
	if err := s.verifyDownstream(ctx); err != nil {
		return domain.Operation{}, err
	}
	if stored.BindingID == "" {
		return s.applyCreate(ctx, stored, idempotencyKey, semanticPayload)
	}
	return s.applyUpdate(ctx, stored, idempotencyKey, semanticPayload)
}

func (s *Service) Status(ctx context.Context, principalID, operationID string) (domain.Operation, error) {
	if err := s.authorize(principalID); err != nil {
		return domain.Operation{}, err
	}
	record, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return domain.Operation{}, mapStorageError(err, "load operation")
	}
	return record.Operation, nil
}

// Recover never retries an uncertain write. It only resumes a definitely
// unsent prepared operation, or reads a target identified by an already
// persisted trusted receipt/allow-list and compares it with baseline/expected.
func (s *Service) Recover(ctx context.Context, principalID, operationID string) (domain.Operation, error) {
	if err := s.authorize(principalID); err != nil {
		return domain.Operation{}, err
	}
	record, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return domain.Operation{}, mapStorageError(err, "load operation")
	}
	if terminal(record.Operation.Status) {
		return record.Operation, nil
	}
	if err := s.verifyDownstream(ctx); err != nil {
		return record.Operation, err
	}
	plan, err := s.store.GetPlan(ctx, record.PlanID)
	if err != nil {
		return record.Operation, mapStorageError(err, "load operation plan")
	}
	if record.Operation.Status == domain.OperationPrepared {
		if err := s.validatePlan(plan); err != nil {
			return s.failOperation(ctx, record, "stale_prepared_operation", err)
		}
		if plan.ConfirmationRequired && plan.ConfirmedAt == nil {
			return s.failOperation(ctx, record, "confirmation_required", storage.ErrConfirmationRequired)
		}
		if plan.BindingID != "" {
			state, stateErr := s.store.GetBindingState(ctx, plan.BindingID)
			if stateErr != nil || state.Binding.Version != plan.Plan.BindingVersion {
				return s.failOperation(ctx, record, "binding_version_changed", storage.ErrBindingVersionChanged)
			}
		}
		if record.Kind == storage.OperationCreate {
			return s.executeCreate(ctx, record, plan)
		}
		if plan.Plan.Status == "no_change" {
			return s.executeNoChange(ctx, record, plan)
		}
		return s.executeUpdate(ctx, record, plan)
	}
	if record.Operation.Status == domain.OperationApplying || record.Operation.Status == domain.OperationVerifying {
		record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
			[]domain.OperationStatus{domain.OperationApplying, domain.OperationVerifying},
			domain.OperationNeedsReconciliation, storage.TransitionPatch{NeedsReadback: boolPointer(true), ErrorCode: "interrupted_write"})
		if err != nil {
			return record.Operation, mapStorageError(err, "mark interrupted operation")
		}
	}
	if record.Operation.Status != domain.OperationNeedsReconciliation {
		return record.Operation, nil
	}
	if record.Kind == storage.OperationCreate {
		return s.reconcileCreate(ctx, record, plan)
	}
	return s.reconcileUpdate(ctx, record, plan)
}

func (s *Service) applyCreate(ctx context.Context, plan storage.StoredPlan, idempotency, semanticPayload string) (domain.Operation, error) {
	record, created, err := s.store.PrepareCreate(ctx, storage.CreateRequest{
		OwnerID: s.installation.OwnerID, OperationID: newID("op"), IdempotencyKey: idempotency, PlanID: plan.Plan.ID,
		PayloadHash: semanticPayload, SourceSnapshotHash: plan.SourceSnapshotHash,
	})
	if err != nil {
		return record.Operation, mapStorageError(err, "prepare create")
	}
	if !created || record.Operation.Status != domain.OperationPrepared {
		return record.Operation, nil
	}
	return s.executeCreate(ctx, record, plan)
}

func (s *Service) executeCreate(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan) (domain.Operation, error) {
	source, err := s.store.GetSourceSnapshot(ctx, plan.SourceSnapshotHash)
	if err != nil {
		return record.Operation, mapStorageError(err, "load planned source")
	}
	desired, err := owu.BuildSnapshot(source, nil)
	if err != nil {
		return s.failOperation(ctx, record, "invalid_planned_target", err)
	}
	if !matchesPlanned(desired, plan) {
		return s.failOperation(ctx, record, "plan_payload_changed", errors.New("planned target no longer reproduces"))
	}
	record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationPrepared}, domain.OperationApplying,
		storage.TransitionPatch{NeedsReadback: boolPointer(false)})
	if err != nil {
		return record.Operation, mapStorageError(err, "begin create")
	}
	receipt, err := s.target.Create(ctx, owu.CreateRequest{OperationID: record.Operation.ID, Snapshot: desired})
	if err != nil {
		if owu.IsOutcomeUnknown(err) {
			return s.unknownOperation(ctx, record, "create_outcome_unknown", "the create result is unknown and no trusted target receipt exists")
		}
		return s.failOperation(ctx, record, "create_failed", err)
	}
	if receipt.TargetID == "" || receipt.Snapshot.ChatID != receipt.TargetID {
		return s.unknownOperation(ctx, record, "create_receipt_untrusted", "the create receipt did not identify a target")
	}
	if receipt.Snapshot.OwnerID != s.installation.OWUAccountID {
		return s.unknownOperation(ctx, record, "create_receipt_untrusted", "the create receipt belongs to an unexpected account")
	}
	// Persist the trusted receipt before the first targeted read. If this write
	// fails, the target is deliberately not read or adopted later.
	record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationApplying}, domain.OperationVerifying,
		storage.TransitionPatch{TargetChatID: receipt.TargetID, RemoteReceiptID: receipt.TargetID, NeedsReadback: boolPointer(true)})
	if err != nil {
		return record.Operation, mapStorageError(err, "persist create receipt")
	}
	trustedTargetID, err := s.store.GetTrustedCreateTarget(ctx, record.Operation.ID)
	if err != nil {
		return record.Operation, mapStorageError(err, "validate trusted create target")
	}
	actual, err := s.target.Read(ctx, trustedTargetID)
	if err != nil {
		return s.unknownOperation(ctx, record, "create_readback_failed", "the trusted created target could not yet be verified")
	}
	return s.finishCreate(ctx, record, plan, source, actual)
}

func (s *Service) finishCreate(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan, source domain.SourceSnapshot, actual owu.Snapshot) (domain.Operation, error) {
	if actual.ChatID != record.TargetChatID || actual.OwnerID != s.installation.OWUAccountID {
		return s.unknownOperation(ctx, record, "create_owner_mismatch", "the created target readback has an unexpected owner")
	}
	if !owu.HasOperationMarker(actual, record.Operation.ID, true) || !matchesPlanned(actual, plan) {
		return s.unknownOperation(ctx, record, "create_verification_mismatch", "the created target differs from the confirmed plan")
	}
	if record.Operation.Status == domain.OperationNeedsReconciliation {
		var err error
		record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
			[]domain.OperationStatus{domain.OperationNeedsReconciliation}, domain.OperationVerifying,
			storage.TransitionPatch{NeedsReadback: boolPointer(true)})
		if err != nil {
			return record.Operation, mapStorageError(err, "resume verified create finalization")
		}
	}
	bindingID := newID("binding")
	_, err := s.store.FinalizeVerifiedCreate(ctx, storage.VerifiedCreate{
		OperationID: record.Operation.ID, BindingID: bindingID, TargetChatID: record.TargetChatID,
		RemoteReceiptID: record.Operation.RemoteReceiptID, SourceIdentity: source.Identity,
		MessageMappings: mappings(actual.Normalized), TitlePolicy: domain.TitleFollowSource,
		VerifiedTarget: actual.Normalized, VerifiedConflictHash: actual.ConflictHash,
	})
	if err != nil {
		return record.Operation, mapStorageError(err, "finalize verified create")
	}
	finished, err := s.store.GetOperation(ctx, record.Operation.ID)
	if err != nil {
		return record.Operation, mapStorageError(err, "load completed create")
	}
	return finished.Operation, nil
}

func (s *Service) applyUpdate(ctx context.Context, plan storage.StoredPlan, idempotency, semanticPayload string) (domain.Operation, error) {
	state, err := s.store.GetBindingState(ctx, plan.BindingID)
	if err != nil {
		return domain.Operation{}, mapStorageError(err, "load binding")
	}
	if err := s.validateBinding(state.Binding); err != nil {
		return domain.Operation{}, err
	}
	if state.Binding.Version != plan.Plan.BindingVersion {
		return domain.Operation{}, resultError("binding_version_changed", "the binding changed after preview", "create a fresh preview", false, "")
	}
	record, created, err := s.store.PrepareBindingOperation(ctx, storage.PrepareOperationRequest{
		OwnerID: s.installation.OwnerID, OperationID: newID("op"), IdempotencyKey: idempotency, Kind: storage.OperationUpdate,
		BindingID: plan.BindingID, PlanID: plan.Plan.ID, PayloadHash: semanticPayload,
	})
	if err != nil {
		return record.Operation, mapStorageError(err, "prepare update")
	}
	if !created || record.Operation.Status != domain.OperationPrepared {
		return record.Operation, nil
	}
	if plan.Plan.Status == "no_change" {
		return s.executeNoChange(ctx, record, plan)
	}
	return s.executeUpdate(ctx, record, plan)
}

func (s *Service) executeUpdate(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan) (domain.Operation, error) {
	state, err := s.store.GetBindingState(ctx, plan.BindingID)
	if err != nil {
		return record.Operation, mapStorageError(err, "load binding")
	}
	if state.Binding.Version != plan.Plan.BindingVersion {
		return s.failOperation(ctx, record, "binding_version_changed", storage.ErrBindingVersionChanged)
	}
	actual, err := s.target.Read(ctx, state.Binding.TargetChatID)
	if err != nil {
		return s.failOperation(ctx, record, "target_reread_failed", err)
	}
	if actual.ChatID != state.Binding.TargetChatID || actual.OwnerID != s.installation.OWUAccountID {
		return s.failOperation(ctx, record, "target_identity_mismatch", errors.New("target owner changed"))
	}
	if !matchesBaseline(actual, state.TargetBaseline) || actual.ConflictHash != plan.ExpectedTargetConflictHash {
		return s.failOperation(ctx, record, "target_conflict", errors.New("target diverged after preview"))
	}
	source, err := s.store.GetSourceSnapshot(ctx, plan.SourceSnapshotHash)
	if err != nil {
		return record.Operation, mapStorageError(err, "load planned source")
	}
	desired, err := owu.BuildSnapshot(source, &actual)
	if err != nil {
		return s.failOperation(ctx, record, "invalid_planned_target", err)
	}
	if !matchesPlanned(desired, plan) {
		return s.failOperation(ctx, record, "plan_payload_changed", errors.New("planned target no longer reproduces"))
	}
	record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationPrepared}, domain.OperationApplying,
		storage.TransitionPatch{TargetChatID: state.Binding.TargetChatID, NeedsReadback: boolPointer(false)})
	if err != nil {
		return record.Operation, mapStorageError(err, "begin update")
	}
	receipt, err := s.target.Update(ctx, state.Binding.TargetChatID, owu.UpdateRequest{OperationID: record.Operation.ID, Snapshot: desired})
	if err != nil {
		if owu.IsOutcomeUnknown(err) {
			return s.unknownOperation(ctx, record, "update_outcome_unknown", "the update result is unknown; recovery will only read and compare")
		}
		return s.failOperation(ctx, record, "update_failed", err)
	}
	if receipt.Snapshot.ChatID != state.Binding.TargetChatID || receipt.Snapshot.OwnerID != s.installation.OWUAccountID {
		return s.unknownOperation(ctx, record, "update_receipt_untrusted", "the update receipt has an unexpected target identity")
	}
	record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationApplying}, domain.OperationVerifying,
		storage.TransitionPatch{RemoteReceiptID: record.Operation.ID, NeedsReadback: boolPointer(true)})
	if err != nil {
		return record.Operation, mapStorageError(err, "persist update receipt")
	}
	readback, err := s.target.Read(ctx, state.Binding.TargetChatID)
	if err != nil {
		return s.unknownOperation(ctx, record, "update_readback_failed", "the updated target could not yet be verified")
	}
	return s.finishUpdate(ctx, record, plan, state, readback)
}

func (s *Service) executeNoChange(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan) (domain.Operation, error) {
	state, err := s.store.GetBindingState(ctx, plan.BindingID)
	if err != nil {
		return record.Operation, mapStorageError(err, "load binding")
	}
	if state.Binding.Version != plan.Plan.BindingVersion {
		return s.failOperation(ctx, record, "binding_version_changed", storage.ErrBindingVersionChanged)
	}
	actual, err := s.target.Read(ctx, state.Binding.TargetChatID)
	if err != nil {
		return s.failOperation(ctx, record, "target_reread_failed", err)
	}
	if actual.ChatID != state.Binding.TargetChatID || actual.OwnerID != s.installation.OWUAccountID || !matchesBaseline(actual, state.TargetBaseline) ||
		actual.ConflictHash != plan.ExpectedTargetConflictHash {
		return s.failOperation(ctx, record, "target_conflict", errors.New("target diverged after preview"))
	}
	record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationPrepared}, domain.OperationSucceeded,
		storage.TransitionPatch{NeedsReadback: boolPointer(false), VerifiedAt: timePointer(s.now().UTC())})
	if err != nil {
		return record.Operation, mapStorageError(err, "complete no-change operation")
	}
	return record.Operation, nil
}

func (s *Service) finishUpdate(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan, state storage.BindingState, actual owu.Snapshot) (domain.Operation, error) {
	if actual.ChatID != state.Binding.TargetChatID || actual.OwnerID != s.installation.OWUAccountID {
		return s.unknownOperation(ctx, record, "update_owner_mismatch", "the target readback has an unexpected owner")
	}
	if !owu.HasOperationMarker(actual, record.Operation.ID, false) || !matchesPlanned(actual, plan) {
		return s.unknownOperation(ctx, record, "update_verification_mismatch", "the target differs from the confirmed plan")
	}
	if record.Operation.Status == domain.OperationNeedsReconciliation {
		var err error
		record, err = s.store.TransitionOperation(ctx, record.Operation.ID,
			[]domain.OperationStatus{domain.OperationNeedsReconciliation}, domain.OperationVerifying,
			storage.TransitionPatch{NeedsReadback: boolPointer(true)})
		if err != nil {
			return record.Operation, mapStorageError(err, "resume verified update completion")
		}
	}
	hash, err := actual.Normalized.ComputeHash()
	if err != nil {
		return record.Operation, err
	}
	baseline := storage.TargetBaseline{
		BindingID: state.Binding.ID, Snapshot: actual.Normalized, SnapshotHash: hash,
		ConflictHash: actual.ConflictHash, SourceSnapshotHash: plan.SourceSnapshotHash,
		BindingVersion: state.Binding.Version + 1, OperationID: record.Operation.ID, VerifiedAt: s.now().UTC(),
	}
	if err := s.store.CompleteVerifiedUpdate(ctx, record.Operation.ID, baseline, state.Binding.Version); err != nil {
		return record.Operation, mapStorageError(err, "complete verified update")
	}
	finished, err := s.store.GetOperation(ctx, record.Operation.ID)
	if err != nil {
		return record.Operation, mapStorageError(err, "load completed update")
	}
	return finished.Operation, nil
}

func (s *Service) reconcileCreate(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan) (domain.Operation, error) {
	// An unknown create without a durably recorded trustworthy receipt cannot
	// be targeted safely. It remains paused across every recovery attempt.
	if record.TargetChatID == "" || record.Operation.RemoteReceiptID == "" {
		return record.Operation, resultError("create_outcome_unknown", "the create result has no trustworthy target receipt", "inspect the dedicated test account manually; this operation will not be retried or adopted by search", false, record.Operation.ID)
	}
	trustedTargetID, err := s.store.GetTrustedCreateTarget(ctx, record.Operation.ID)
	if err != nil {
		return record.Operation, mapStorageError(err, "validate trusted create target")
	}
	actual, err := s.target.Read(ctx, trustedTargetID)
	if err != nil {
		return record.Operation, resultError("reconciliation_read_failed", "the trusted target receipt could not be read", "retry recovery after Open WebUI is available", true, record.Operation.ID)
	}
	source, err := s.store.GetSourceSnapshot(ctx, plan.SourceSnapshotHash)
	if err != nil {
		return record.Operation, mapStorageError(err, "load planned source")
	}
	return s.finishCreate(ctx, record, plan, source, actual)
}

func (s *Service) reconcileUpdate(ctx context.Context, record storage.OperationRecord, plan storage.StoredPlan) (domain.Operation, error) {
	state, err := s.store.GetBindingState(ctx, plan.BindingID)
	if err != nil {
		return record.Operation, mapStorageError(err, "load binding")
	}
	actual, err := s.target.Read(ctx, state.Binding.TargetChatID)
	if err != nil {
		return record.Operation, resultError("reconciliation_read_failed", "the managed target could not be read", "retry recovery after Open WebUI is available", true, record.Operation.ID)
	}
	if actual.ChatID != state.Binding.TargetChatID || actual.OwnerID != s.installation.OWUAccountID {
		return record.Operation, resultError("target_identity_mismatch", "the target readback has an unexpected identity", "restore the configured target; recovery will not write", false, record.Operation.ID)
	}
	if owu.HasOperationMarker(actual, record.Operation.ID, false) && matchesPlanned(actual, plan) {
		return s.finishUpdate(ctx, record, plan, state, actual)
	}
	if matchesBaseline(actual, state.TargetBaseline) {
		return record.Operation, resultError("update_still_uncertain", "the target still matches the old baseline, but the earlier write may complete later", "keep this operation paused and check recovery again; the service will not write or release the binding", false, record.Operation.ID)
	}
	return record.Operation, resultError("reconciliation_conflict", "the target matches neither the planned result nor the verified baseline", "resolve the target conflict manually; recovery will not write", false, record.Operation.ID)
}

func (s *Service) failOperation(ctx context.Context, record storage.OperationRecord, code string, cause error) (domain.Operation, error) {
	updated, err := s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationPrepared, domain.OperationApplying, domain.OperationVerifying, domain.OperationNeedsReconciliation},
		domain.OperationFailed, storage.TransitionPatch{NeedsReadback: boolPointer(false), ErrorCode: code})
	if err != nil {
		return record.Operation, mapStorageError(err, "persist failed operation")
	}
	_ = cause // never expose downstream bodies or addresses through this layer
	return updated.Operation, resultError(code, "the sync operation failed without a verified result", "create a fresh preview after correcting the reported condition", false, updated.Operation.ID)
}

func (s *Service) unknownOperation(ctx context.Context, record storage.OperationRecord, code, message string) (domain.Operation, error) {
	updated, err := s.store.TransitionOperation(ctx, record.Operation.ID,
		[]domain.OperationStatus{domain.OperationApplying, domain.OperationVerifying, domain.OperationNeedsReconciliation},
		domain.OperationNeedsReconciliation, storage.TransitionPatch{NeedsReadback: boolPointer(true), ErrorCode: code})
	if err != nil {
		return record.Operation, mapStorageError(err, "persist uncertain operation")
	}
	return updated.Operation, resultError(code, message, "use status or recovery; the service will not blindly retry the write", false, updated.Operation.ID)
}

func (s *Service) authorize(principalID string) error {
	if s.store == nil {
		return resultError("storage_unavailable", "persistent storage is unavailable", "restore the data store", false, "")
	}
	if principalID == "" || principalID != s.installation.OwnerID {
		return resultError("unauthorized_owner", "the caller is not the fixed installation owner", "use the initialized local owner identity", false, "")
	}
	return nil
}

func (s *Service) verifyDownstream(ctx context.Context) error {
	if s.target == nil {
		return resultError("target_unavailable", "Open WebUI is not configured for this operation", "configure the fixed Open WebUI target", false, "")
	}
	identity, err := s.target.VerifyIdentity(ctx)
	if err != nil {
		return resultError("target_identity_unavailable", "the fixed Open WebUI identity could not be verified", "restore the configured Open WebUI site and account", true, "")
	}
	if identity.SiteID != s.installation.OWUSiteID || identity.AccountID != s.installation.OWUAccountID {
		return resultError("target_identity_mismatch", "the live Open WebUI site or account differs from the installation identity", "restore the configured site and account; no managed target was read or written", false, "")
	}
	return nil
}

func (s *Service) validateBinding(binding domain.Binding) error {
	if binding.InstallationID != s.installation.ID || binding.PrincipalID != s.installation.OwnerID ||
		binding.OWUSiteID != s.installation.OWUSiteID || binding.OWUAccountID != s.installation.OWUAccountID ||
		binding.TargetChatID == "" || binding.CreatedByOperation == "" {
		return resultError("untrusted_binding", "the binding lacks trusted creation and fixed identity evidence", "restore the original database from backup", false, "")
	}
	return nil
}

func (s *Service) validatePlan(plan storage.StoredPlan) error {
	if err := s.validatePlanOwnership(plan); err != nil {
		return err
	}
	now := s.now().UTC()
	if plan.Plan.ID == "" || plan.Plan.ID != strings.TrimSpace(plan.Plan.ID) || plan.PayloadHash == "" {
		return resultError("invalid_plan", "the persisted plan is incomplete", "create a fresh preview", false, "")
	}
	if !plan.Plan.ExpiresAt.After(now) {
		return resultError("plan_expired", "the sync plan has expired", "create a fresh preview", false, "")
	}
	if plan.Plan.ParserVersion != domain.ParserVersion || plan.Plan.PolicyVersion != domain.PolicyVersion {
		return resultError("plan_version_changed", "the parser or policy version changed after preview", "create a fresh preview", false, "")
	}
	if plan.Plan.Status != "ready" && plan.Plan.Status != "no_change" && plan.Plan.Status != "needs_binding_confirmation" {
		return resultError("plan_not_applicable", "the sync plan contains an unresolved conflict or unsupported content", "resolve the preview result and create a new plan", false, "")
	}
	return nil
}

func (s *Service) validatePlanOwnership(plan storage.StoredPlan) error {
	if plan.OwnerID != s.installation.OwnerID || plan.SourceSnapshotHash == "" ||
		plan.Plan.SourceSnapshotHash != plan.SourceSnapshotHash {
		return resultError("invalid_plan_owner", "the persisted plan is not owned by this installation", "restore the original database from backup", false, "")
	}
	if (plan.BindingID == "" && plan.Plan.BindingVersion != 0) ||
		(plan.BindingID != "" && plan.Plan.BindingVersion == 0) ||
		(plan.BindingID != "" && plan.Plan.TargetBaselineHash != plan.ExpectedTargetHash) {
		return resultError("invalid_plan_binding", "the persisted plan binding evidence is inconsistent", "create a fresh preview", false, "")
	}
	return nil
}

func makePersistedPlan(plan domain.SyncPlan, now time.Time, ttl time.Duration) domain.SyncPlan {
	plan.ID = newID("plan")
	plan.ExpiresAt = now.Add(ttl)
	return plan
}

func makeStoredPlan(plan domain.SyncPlan, bindingID string, source domain.SourceSnapshot, previous *domain.SourceSnapshot, actual *owu.Snapshot, desired owu.Snapshot, now time.Time, confirmationRequired bool) (storage.StoredPlan, error) {
	desiredTarget := desired.Normalized
	desiredTarget.BusinessHash = ""
	stored := storage.StoredPlan{
		Plan: plan, BindingID: bindingID, SourceSnapshotHash: source.BusinessHash,
		DesiredTarget: desiredTarget, DesiredConflictHash: desired.ConflictHash,
		DesiredRawChat:     append([]byte(nil), desired.RawChat...),
		DesiredRawEnvelope: append([]byte(nil), desired.RawEnvelope...),
		DesiredArchived:    desired.Archived, DesiredPinned: desired.Pinned, DesiredFolderID: desired.FolderID,
		ConfirmationRequired: confirmationRequired, CreatedAt: now,
	}
	if previous != nil {
		stored.PreviousSnapshotHash = previous.BusinessHash
	}
	if actual != nil {
		hash, err := actual.Normalized.ComputeHash()
		if err != nil {
			return storage.StoredPlan{}, err
		}
		stored.ExpectedTargetHash = hash
		stored.ExpectedTargetConflictHash = actual.ConflictHash
	}
	return stored, nil
}

func plannedSnapshot(plan storage.StoredPlan) owu.Snapshot {
	return owu.Snapshot{
		Normalized:   plan.DesiredTarget,
		RawChat:      append([]byte(nil), plan.DesiredRawChat...),
		RawEnvelope:  append([]byte(nil), plan.DesiredRawEnvelope...),
		ConflictHash: plan.DesiredConflictHash,
		Archived:     plan.DesiredArchived, Pinned: plan.DesiredPinned, FolderID: plan.DesiredFolderID,
	}
}

func matchesPlanned(actual owu.Snapshot, plan storage.StoredPlan) bool {
	return actual.Archived == plan.DesiredArchived && actual.Pinned == plan.DesiredPinned &&
		actual.FolderID == plan.DesiredFolderID && owu.MatchSnapshot(actual, plannedSnapshot(plan))
}

func (s *Service) operationIdentity(ctx context.Context, plan storage.StoredPlan) (string, string, error) {
	var key string
	if plan.BindingID == "" {
		source, err := s.store.GetSourceSnapshot(ctx, plan.SourceSnapshotHash)
		if err != nil {
			return "", "", mapStorageError(err, "load planned source")
		}
		key = s.createIdempotencyKey(source)
	} else {
		key = s.updateIdempotencyKey(plan)
	}
	payload := struct {
		Kind                       storage.OperationKind
		BindingID                  string
		SourceSnapshotHash         string
		PreviousSnapshotHash       string
		ExpectedTargetHash         string
		ExpectedTargetConflictHash string
		DesiredTarget              domain.TargetSnapshot
		DesiredRawChat             json.RawMessage
		DesiredArchived            bool
		DesiredPinned              bool
		DesiredFolderID            string
		ParserVersion              string
		PolicyVersion              string
	}{
		Kind: storage.OperationUpdate, BindingID: plan.BindingID,
		SourceSnapshotHash: plan.SourceSnapshotHash, PreviousSnapshotHash: plan.PreviousSnapshotHash,
		ExpectedTargetHash: plan.ExpectedTargetHash, ExpectedTargetConflictHash: plan.ExpectedTargetConflictHash,
		DesiredTarget: plan.DesiredTarget, DesiredRawChat: plan.DesiredRawChat,
		DesiredArchived: plan.DesiredArchived, DesiredPinned: plan.DesiredPinned, DesiredFolderID: plan.DesiredFolderID,
		ParserVersion: plan.Plan.ParserVersion, PolicyVersion: plan.Plan.PolicyVersion,
	}
	if plan.BindingID == "" {
		payload.Kind = storage.OperationCreate
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(encoded)
	return key, hex.EncodeToString(sum[:]), nil
}

func normalizeSource(snapshot *domain.SourceSnapshot) error {
	hash, err := snapshot.ComputeHash()
	if err != nil {
		return err
	}
	if snapshot.BusinessHash != "" && snapshot.BusinessHash != hash {
		return errors.New("source business hash mismatch")
	}
	if snapshot.ParserVersion != domain.ParserVersion || snapshot.SourceType != domain.SourceChatGPTShare {
		return errors.New("unsupported source parser or type")
	}
	snapshot.BusinessHash = hash
	return nil
}

func verifiedSameSource(previous, current domain.SourceIdentity) bool {
	return previous.Evidence == domain.EvidenceVerified && current.Evidence == domain.EvidenceVerified &&
		previous.CandidateID != "" && previous.CandidateID == current.CandidateID
}

func matchesBaseline(actual owu.Snapshot, baseline storage.TargetBaseline) bool {
	if baseline.ConflictHash == "" || actual.ConflictHash != baseline.ConflictHash {
		return false
	}
	return targetEqual(actual.Normalized, baseline.Snapshot)
}

func targetEqual(left, right domain.TargetSnapshot) bool {
	left.BusinessHash = ""
	right.BusinessHash = ""
	left.OfflineSimulation = false
	right.OfflineSimulation = false
	return reflect.DeepEqual(left, right)
}

func mappings(target domain.TargetSnapshot) []domain.MessageMapping {
	result := make([]domain.MessageMapping, 0, len(target.Messages))
	for _, message := range target.Messages {
		if message.Managed && message.SourceID != "" && message.ID != "" {
			result = append(result, domain.MessageMapping{SourceMessageID: message.SourceID, TargetMessageID: message.ID})
		}
	}
	return result
}

func (s *Service) createIdempotencyKey(source domain.SourceSnapshot) string {
	identity := source.Identity.CandidateID
	if identity == "" {
		identity = source.BusinessHash
	}
	return digest("create", s.installation.ID, s.installation.OwnerID, identity, domain.PolicyVersion)
}

func (s *Service) updateIdempotencyKey(plan storage.StoredPlan) string {
	return digest("update", s.installation.ID, s.installation.OwnerID, plan.BindingID,
		fmt.Sprintf("%d", plan.Plan.BindingVersion), plan.SourceSnapshotHash, domain.PolicyVersion)
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func newID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func terminal(status domain.OperationStatus) bool {
	return status == domain.OperationSucceeded || status == domain.OperationFailed
}

func boolPointer(value bool) *bool           { return &value }
func timePointer(value time.Time) *time.Time { return &value }

func resultError(code, message, next string, retryable bool, operationID string) *domain.ResultError {
	return &domain.ResultError{Code: code, Message: message, NextAction: next, Retryable: retryable, OperationID: operationID}
}

func mapStorageError(err error, action string) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return resultError("not_found", action+" failed because the record does not exist", "use a persisted plan, binding, or operation id", false, "")
	case errors.Is(err, storage.ErrIdempotencyConflict), errors.Is(err, storage.ErrImmutableConflict):
		return resultError("idempotency_conflict", "an existing immutable request has different content", "inspect the existing operation; do not retry the write with changed content", false, "")
	case errors.Is(err, storage.ErrOperationInProgress):
		return resultError("operation_in_progress", "an unresolved operation blocks another write", "recover the existing operation first", false, "")
	case errors.Is(err, storage.ErrUntrustedTarget):
		return resultError("untrusted_target", "the requested binding is not backed by a verified creation record", "use only a binding created and verified by this installation", false, "")
	case errors.Is(err, storage.ErrPlanExpired):
		return resultError("plan_expired", "the sync plan expired", "create a fresh preview", false, "")
	case errors.Is(err, storage.ErrConfirmationRequired):
		return resultError("confirmation_required", "the sync plan has not been confirmed", "review and explicitly confirm the immutable plan", false, "")
	case errors.Is(err, storage.ErrBindingVersionChanged):
		return resultError("binding_version_changed", "the binding changed after preview", "create a fresh preview", false, "")
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
