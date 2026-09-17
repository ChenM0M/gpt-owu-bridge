package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

func (s *Store) PrepareCreate(ctx context.Context, request CreateRequest) (OperationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.OwnerID != "" && request.OwnerID != s.installation.OwnerID {
		return OperationRecord{}, false, ErrIdentityMismatch
	}
	if strings.TrimSpace(request.OperationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		strings.TrimSpace(request.PayloadHash) == "" {
		return OperationRecord{}, false, errors.New("operation ID, idempotency key, and execution payload hash are required")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return OperationRecord{}, false, fmt.Errorf("begin create preparation: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	if existing, found, err := getOperationByIdempotency(ctx, tx, request.IdempotencyKey); err != nil {
		return OperationRecord{}, false, err
	} else if found {
		if operationMatches(existing, OperationCreate, "", request.PayloadHash) {
			return existing, false, nil
		}
		return existing, false, ErrIdempotencyConflict
	}
	plan, err := getPlan(ctx, tx, request.PlanID)
	if err != nil {
		return OperationRecord{}, false, err
	}
	if plan.BindingID != "" || plan.Plan.BindingVersion != 0 {
		return OperationRecord{}, false, errors.New("create plan cannot reference an existing binding")
	}
	if request.SourceSnapshotHash != "" && request.SourceSnapshotHash != plan.SourceSnapshotHash {
		return OperationRecord{}, false, ErrImmutableConflict
	}
	if err := validateExecutablePlan(plan, time.Now().UTC()); err != nil {
		return OperationRecord{}, false, err
	}
	snapshot, err := getSourceSnapshot(ctx, tx, plan.SourceSnapshotHash)
	if err != nil {
		return OperationRecord{}, false, err
	}
	alias, err := sourceAlias(snapshot)
	if err != nil {
		return OperationRecord{}, false, err
	}
	var claimedOperationID string
	err = tx.QueryRowContext(ctx,
		"SELECT operation_id FROM creation_intents WHERE source_alias = ?", alias,
	).Scan(&claimedOperationID)
	if err == nil {
		existing, getErr := getOperation(ctx, tx, claimedOperationID)
		if getErr != nil {
			return OperationRecord{}, false, getErr
		}
		return existing, false, ErrSourceAlreadyClaimed
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return OperationRecord{}, false, fmt.Errorf("check source creation claim: %w", err)
	}
	lockKey := "create:" + hashText(alias)
	operation, err := insertPreparedOperation(ctx, tx, s.installation.OwnerID,
		request.OperationID, request.IdempotencyKey, OperationCreate, "", lockKey,
		plan.Plan.ID, request.PayloadHash)
	if err != nil {
		return OperationRecord{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO creation_intents(operation_id, source_snapshot_hash, source_alias, created_at_ns)
		VALUES (?, ?, ?, ?)`, operation.Operation.ID, plan.SourceSnapshotHash, alias,
		operation.CreatedAt.UnixNano()); err != nil {
		return OperationRecord{}, false, fmt.Errorf("persist trusted create intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperationRecord{}, false, fmt.Errorf("commit create preparation: %w", err)
	}
	rollback = false
	return operation, true, nil
}

func (s *Store) PrepareBindingOperation(ctx context.Context, request PrepareOperationRequest) (OperationRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.OwnerID != "" && request.OwnerID != s.installation.OwnerID {
		return OperationRecord{}, false, ErrIdentityMismatch
	}
	if request.Kind != OperationUpdate && request.Kind != OperationArchive {
		return OperationRecord{}, false, errors.New("binding operation must be update or archive")
	}
	if request.BindingID == "" {
		return OperationRecord{}, false, errors.New("binding ID is required")
	}
	if strings.TrimSpace(request.OperationID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" ||
		strings.TrimSpace(request.PayloadHash) == "" {
		return OperationRecord{}, false, errors.New("operation ID, idempotency key, and execution payload hash are required")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return OperationRecord{}, false, fmt.Errorf("begin binding operation preparation: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	if existing, found, err := getOperationByIdempotency(ctx, tx, request.IdempotencyKey); err != nil {
		return OperationRecord{}, false, err
	} else if found {
		if operationMatches(existing, request.Kind, request.BindingID, request.PayloadHash) {
			return existing, false, nil
		}
		return existing, false, ErrIdempotencyConflict
	}
	if err := validateAllowedBinding(ctx, tx, s.installation, request.BindingID); err != nil {
		return OperationRecord{}, false, err
	}
	plan, err := getPlan(ctx, tx, request.PlanID)
	if err != nil {
		return OperationRecord{}, false, err
	}
	if plan.BindingID != request.BindingID {
		return OperationRecord{}, false, ErrImmutableConflict
	}
	if err := validateExecutablePlan(plan, time.Now().UTC()); err != nil {
		return OperationRecord{}, false, err
	}
	var bindingVersion uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT version FROM bindings WHERE binding_id = ?", request.BindingID,
	).Scan(&bindingVersion); err != nil {
		return OperationRecord{}, false, fmt.Errorf("read binding version: %w", err)
	}
	if bindingVersion != plan.Plan.BindingVersion {
		return OperationRecord{}, false, ErrBindingVersionChanged
	}
	baseline, err := getTargetBaseline(ctx, tx, request.BindingID, bindingVersion)
	if err != nil {
		return OperationRecord{}, false, err
	}
	if plan.ExpectedTargetHash != "" && plan.ExpectedTargetHash != baseline.SnapshotHash {
		return OperationRecord{}, false, ErrImmutableConflict
	}
	if plan.ExpectedTargetConflictHash != "" && plan.ExpectedTargetConflictHash != baseline.ConflictHash {
		return OperationRecord{}, false, ErrImmutableConflict
	}
	operation, err := insertPreparedOperation(ctx, tx, s.installation.OwnerID,
		request.OperationID, request.IdempotencyKey, request.Kind, request.BindingID,
		"binding:"+request.BindingID, plan.Plan.ID, request.PayloadHash)
	if err != nil {
		return OperationRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return OperationRecord{}, false, fmt.Errorf("commit binding operation preparation: %w", err)
	}
	rollback = false
	return operation, true, nil
}

func insertPreparedOperation(ctx context.Context, tx *sql.Tx, ownerID, operationID, idempotencyKey string,
	kind OperationKind, bindingID, lockKey, planID, payloadHash string,
) (OperationRecord, error) {
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(idempotencyKey) == "" {
		return OperationRecord{}, errors.New("operation ID and idempotency key are required")
	}
	var blocker string
	err := tx.QueryRowContext(ctx, `
		SELECT operation_id FROM operations
		WHERE lock_key = ? AND status IN ('prepared','applying','verifying','needs_reconciliation')`,
		lockKey).Scan(&blocker)
	if err == nil {
		return OperationRecord{}, fmt.Errorf("%w: %s", ErrOperationInProgress, blocker)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return OperationRecord{}, fmt.Errorf("check operation lock: %w", err)
	}
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO operations(
			operation_id, owner_id, idempotency_key, kind, binding_id, lock_key,
			plan_id, payload_hash, status, attempt_count, needs_readback,
			last_error_code, remote_receipt_id, target_chat_id, created_at_ns, updated_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'prepared', 0, 0, '', '', '', ?, ?)`,
		operationID, ownerID, idempotencyKey, string(kind), bindingID, lockKey,
		planID, payloadHash, now.UnixNano(), now.UnixNano())
	if err != nil {
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "one_unresolved_operation_per_lock") || strings.Contains(lower, "lock_key") {
			return OperationRecord{}, ErrOperationInProgress
		}
		return OperationRecord{}, fmt.Errorf("save prepared operation: %w", err)
	}
	return OperationRecord{
		Operation: domain.Operation{ID: operationID, IdempotencyKey: idempotencyKey,
			BindingID: bindingID, Status: domain.OperationPrepared},
		Kind: kind, PlanID: planID, PayloadHash: payloadHash,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func validateExecutablePlan(plan StoredPlan, now time.Time) error {
	if plan.OwnerID == "" {
		return ErrIdentityMismatch
	}
	if !now.Before(plan.Plan.ExpiresAt) {
		return ErrPlanExpired
	}
	if plan.ConfirmationRequired && plan.ConfirmedAt == nil {
		return ErrConfirmationRequired
	}
	return nil
}

func (s *Store) GetOperation(ctx context.Context, id string) (OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return getOperation(ctx, s.conn, id)
}

func (s *Store) GetOperationByIdempotencyKey(ctx context.Context, key string) (OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, found, err := getOperationByIdempotency(ctx, s.conn, key)
	if err != nil {
		return OperationRecord{}, err
	}
	if !found {
		return OperationRecord{}, ErrNotFound
	}
	return operation, nil
}

// GetTrustedCreateTarget is the only pre-binding path that may authorize a
// targeted create readback. It requires the durable create-intent evidence
// chain and the receipt recorded by the guarded applying->verifying transition.
func (s *Store) GetTrustedCreateTarget(ctx context.Context, operationID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var targetID, receiptID, status, sourceHash, planSourceHash string
	err := s.conn.QueryRowContext(ctx, `
		SELECT o.target_chat_id, o.remote_receipt_id, o.status,
		       ci.source_snapshot_hash, p.source_snapshot_hash
		FROM operations o
		JOIN creation_intents ci ON ci.operation_id = o.operation_id
		JOIN sync_plans p ON p.plan_id = o.plan_id
		JOIN source_snapshots ss ON ss.business_hash = ci.source_snapshot_hash
		WHERE o.operation_id = ? AND o.owner_id = ? AND p.owner_id = ?
		  AND o.kind = 'create' AND o.status IN ('verifying','needs_reconciliation')`,
		operationID, s.installation.OwnerID, s.installation.OwnerID).Scan(
		&targetID, &receiptID, &status, &sourceHash, &planSourceHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUntrustedTarget
	}
	if err != nil {
		return "", fmt.Errorf("validate trusted create target: %w", err)
	}
	if targetID == "" || receiptID == "" || receiptID != targetID || sourceHash != planSourceHash {
		return "", ErrUntrustedTarget
	}
	if _, err := getSourceSnapshot(ctx, s.conn, sourceHash); err != nil {
		return "", fmt.Errorf("validate create source evidence: %w", err)
	}
	var conflicting int
	if err := s.conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM target_allowlist WHERE target_chat_id = ?", targetID,
	).Scan(&conflicting); err != nil {
		return "", fmt.Errorf("check create target conflict: %w", err)
	}
	if conflicting != 0 {
		return "", ErrUntrustedTarget
	}
	return targetID, nil
}

func getOperation(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (OperationRecord, error) {
	return scanOperation(q.QueryRowContext(ctx, `
		SELECT operation_id, idempotency_key, binding_id, status, attempt_count,
		       needs_readback, last_error_code, remote_receipt_id, verified_at_ns,
		       kind, plan_id, payload_hash, target_chat_id, created_at_ns, updated_at_ns
		FROM operations WHERE operation_id = ?`, id))
}

func getOperationByIdempotency(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (OperationRecord, bool, error) {
	result, err := scanOperation(q.QueryRowContext(ctx, `
		SELECT operation_id, idempotency_key, binding_id, status, attempt_count,
		       needs_readback, last_error_code, remote_receipt_id, verified_at_ns,
		       kind, plan_id, payload_hash, target_chat_id, created_at_ns, updated_at_ns
		FROM operations WHERE idempotency_key = ?`, key))
	if errors.Is(err, ErrNotFound) {
		return OperationRecord{}, false, nil
	}
	return result, err == nil, err
}

type rowScanner interface{ Scan(...any) error }

func scanOperation(row rowScanner) (OperationRecord, error) {
	var result OperationRecord
	var status, kind string
	var needsReadback int
	var verifiedNS sql.NullInt64
	var createdNS, updatedNS int64
	err := row.Scan(&result.Operation.ID, &result.Operation.IdempotencyKey,
		&result.Operation.BindingID, &status, &result.Operation.AttemptCount,
		&needsReadback, &result.Operation.LastErrorCode, &result.Operation.RemoteReceiptID,
		&verifiedNS, &kind, &result.PlanID, &result.PayloadHash, &result.TargetChatID,
		&createdNS, &updatedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationRecord{}, ErrNotFound
	}
	if err != nil {
		return OperationRecord{}, fmt.Errorf("read operation: %w", err)
	}
	result.Operation.Status = domain.OperationStatus(status)
	result.Operation.NeedsReadback = needsReadback != 0
	result.Kind = OperationKind(kind)
	result.CreatedAt = time.Unix(0, createdNS).UTC()
	result.UpdatedAt = time.Unix(0, updatedNS).UTC()
	if verifiedNS.Valid {
		verifiedAt := time.Unix(0, verifiedNS.Int64).UTC()
		result.Operation.VerifiedAt = &verifiedAt
	}
	return result, nil
}

func operationMatches(existing OperationRecord, kind OperationKind, bindingID, payloadHash string) bool {
	if existing.Kind != kind || existing.PayloadHash != payloadHash {
		return false
	}
	return kind == OperationCreate || existing.Operation.BindingID == bindingID
}

func (s *Store) TransitionOperation(ctx context.Context, id string, expected []domain.OperationStatus,
	next domain.OperationStatus, patch TransitionPatch,
) (OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(expected) == 0 {
		return OperationRecord{}, errors.New("expected operation states are required")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return OperationRecord{}, fmt.Errorf("begin operation transition: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	operation, err := getOperation(ctx, tx, id)
	if err != nil {
		return OperationRecord{}, err
	}
	if !slices.Contains(expected, operation.Operation.Status) || !transitionAllowed(operation.Operation.Status, next) {
		return operation, ErrInvalidTransition
	}
	if patch.TargetChatID != "" {
		if operation.TargetChatID != "" && operation.TargetChatID != patch.TargetChatID {
			return operation, ErrImmutableConflict
		}
		operation.TargetChatID = patch.TargetChatID
	}
	if patch.RemoteReceiptID != "" {
		if operation.Operation.RemoteReceiptID != "" && operation.Operation.RemoteReceiptID != patch.RemoteReceiptID {
			return operation, ErrImmutableConflict
		}
		operation.Operation.RemoteReceiptID = patch.RemoteReceiptID
	}
	if operation.Kind == OperationCreate && next == domain.OperationVerifying && operation.TargetChatID == "" {
		return operation, errors.New("create result target must be recorded before verification")
	}
	if patch.NeedsReadback != nil {
		operation.Operation.NeedsReadback = *patch.NeedsReadback
	}
	if patch.ErrorCode != "" {
		operation.Operation.LastErrorCode = patch.ErrorCode
	}
	if patch.VerifiedAt != nil {
		verified := patch.VerifiedAt.UTC()
		operation.Operation.VerifiedAt = &verified
	}
	if next == domain.OperationApplying {
		operation.Operation.AttemptCount++
	}
	if next == domain.OperationNeedsReconciliation {
		operation.Operation.NeedsReadback = true
	}
	operation.Operation.Status = next
	operation.UpdatedAt = time.Now().UTC()
	var verifiedNS any
	if operation.Operation.VerifiedAt != nil {
		verifiedNS = operation.Operation.VerifiedAt.UnixNano()
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE operations SET status = ?, attempt_count = ?, needs_readback = ?,
		       last_error_code = ?, remote_receipt_id = ?, target_chat_id = ?,
		       verified_at_ns = ?, updated_at_ns = ?
		WHERE operation_id = ?`, string(next), operation.Operation.AttemptCount,
		boolInt(operation.Operation.NeedsReadback), operation.Operation.LastErrorCode,
		operation.Operation.RemoteReceiptID, operation.TargetChatID, verifiedNS,
		operation.UpdatedAt.UnixNano(), operation.Operation.ID)
	if err != nil {
		return OperationRecord{}, fmt.Errorf("update operation state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperationRecord{}, fmt.Errorf("commit operation transition: %w", err)
	}
	rollback = false
	return operation, nil
}

func transitionAllowed(from, to domain.OperationStatus) bool {
	switch from {
	case domain.OperationPrepared:
		return to == domain.OperationApplying || to == domain.OperationSucceeded || to == domain.OperationFailed
	case domain.OperationApplying:
		return to == domain.OperationVerifying || to == domain.OperationNeedsReconciliation || to == domain.OperationFailed
	case domain.OperationVerifying:
		return to == domain.OperationSucceeded || to == domain.OperationNeedsReconciliation || to == domain.OperationFailed
	case domain.OperationNeedsReconciliation:
		return to == domain.OperationNeedsReconciliation || to == domain.OperationVerifying || to == domain.OperationFailed
	default:
		return false
	}
}

func (s *Store) RecoverInterrupted(ctx context.Context) ([]OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin interrupted operation recovery: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		UPDATE operations
		SET status = 'needs_reconciliation', needs_readback = 1,
		    last_error_code = CASE WHEN last_error_code = '' THEN 'process_interrupted' ELSE last_error_code END,
		    updated_at_ns = ?
		WHERE status IN ('applying', 'verifying')`, now); err != nil {
		return nil, fmt.Errorf("mark interrupted operations: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT operation_id FROM operations
		WHERE status IN ('prepared','needs_reconciliation')
		ORDER BY created_at_ns, operation_id`)
	if err != nil {
		return nil, fmt.Errorf("list recoverable operations: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan recoverable operation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close recoverable operation rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recoverable operations: %w", err)
	}
	result := make([]OperationRecord, 0, len(ids))
	for _, id := range ids {
		operation, err := getOperation(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, operation)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit interrupted operation recovery: %w", err)
	}
	rollback = false
	return result, nil
}

func (s *Store) ListUnresolvedOperations(ctx context.Context) ([]OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.QueryContext(ctx, `
		SELECT operation_id FROM operations
		WHERE status IN ('prepared','applying','verifying','needs_reconciliation')
		ORDER BY created_at_ns, operation_id`)
	if err != nil {
		return nil, fmt.Errorf("list unresolved operations: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]OperationRecord, 0, len(ids))
	for _, id := range ids {
		operation, err := getOperation(ctx, s.conn, id)
		if err != nil {
			return nil, err
		}
		result = append(result, operation)
	}
	return result, nil
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
