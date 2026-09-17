package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

func (s *Store) FinalizeVerifiedCreate(ctx context.Context, verified VerifiedCreate) (domain.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if verified.OperationID == "" || verified.BindingID == "" || verified.TargetChatID == "" {
		return domain.Binding{}, errors.New("operation, binding, and target chat IDs are required")
	}
	if strings.TrimSpace(verified.VerifiedConflictHash) == "" {
		return domain.Binding{}, errors.New("verified target conflict hash is required")
	}
	if verified.VerifiedTarget.OfflineSimulation {
		return domain.Binding{}, errors.New("offline simulation cannot become a verified target baseline")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("begin verified create finalization: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	operation, err := getOperation(ctx, tx, verified.OperationID)
	if err != nil {
		return domain.Binding{}, err
	}
	if operation.Kind != OperationCreate || operation.Operation.Status != domain.OperationVerifying {
		return domain.Binding{}, ErrInvalidTransition
	}
	if operation.TargetChatID == "" || operation.TargetChatID != verified.TargetChatID {
		return domain.Binding{}, ErrUntrustedTarget
	}
	if operation.Operation.RemoteReceiptID == "" {
		return domain.Binding{}, ErrUntrustedTarget
	}
	if operation.Operation.RemoteReceiptID != "" && verified.RemoteReceiptID != "" &&
		operation.Operation.RemoteReceiptID != verified.RemoteReceiptID {
		return domain.Binding{}, ErrImmutableConflict
	}
	var sourceHash string
	if err := tx.QueryRowContext(ctx, `
		SELECT source_snapshot_hash FROM creation_intents WHERE operation_id = ?`,
		verified.OperationID).Scan(&sourceHash); errors.Is(err, sql.ErrNoRows) {
		return domain.Binding{}, ErrUntrustedTarget
	} else if err != nil {
		return domain.Binding{}, fmt.Errorf("read trusted create intent: %w", err)
	}
	source, err := getSourceSnapshot(ctx, tx, sourceHash)
	if err != nil {
		return domain.Binding{}, err
	}
	if err := validateMappings(source, verified.MessageMappings); err != nil {
		return domain.Binding{}, err
	}
	verified.VerifiedTarget.BusinessHash = ""
	targetHash, err := verified.VerifiedTarget.ComputeHash()
	if err != nil {
		return domain.Binding{}, err
	}
	verified.VerifiedTarget.BusinessHash = targetHash
	targetMappings := mappingsFromTarget(verified.VerifiedTarget)
	if err := validateMappings(source, targetMappings); err != nil {
		return domain.Binding{}, err
	}
	if !sameMappings(verified.MessageMappings, targetMappings) {
		return domain.Binding{}, errors.New("verified target mappings do not match the supplied creation mappings")
	}
	targetJSON, err := json.Marshal(verified.VerifiedTarget)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("marshal verified target: %w", err)
	}
	sourceIdentityJSON, err := json.Marshal(verified.SourceIdentity)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("marshal source identity: %w", err)
	}
	now := time.Now().UTC()
	binding := domain.Binding{
		ID:                 verified.BindingID,
		InstallationID:     s.installation.ID,
		PrincipalID:        s.installation.OwnerID,
		OWUSiteID:          s.installation.OWUSiteID,
		OWUAccountID:       s.installation.OWUAccountID,
		TargetChatID:       verified.TargetChatID,
		CreatedByOperation: verified.OperationID,
		SourceIdentity:     verified.SourceIdentity,
		MessageMappings:    append([]domain.MessageMapping(nil), verified.MessageMappings...),
		TitlePolicy:        verified.TitlePolicy,
		Version:            1,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO bindings(
			binding_id, installation_id, owner_id, owu_site_id, owu_account_id,
			target_chat_id, created_by_operation, source_identity_json, title_policy,
			version, last_source_snapshot_hash, created_at_ns, updated_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		binding.ID, binding.InstallationID, binding.PrincipalID, binding.OWUSiteID,
		binding.OWUAccountID, binding.TargetChatID, binding.CreatedByOperation,
		sourceIdentityJSON, string(binding.TitlePolicy), sourceHash,
		now.UnixNano(), now.UnixNano()); err != nil {
		return domain.Binding{}, fmt.Errorf("install verified binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO target_allowlist(
			target_chat_id, binding_id, installation_id, owner_id, owu_site_id,
			owu_account_id, creation_operation_id, verified_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		binding.TargetChatID, binding.ID, binding.InstallationID, binding.PrincipalID,
		binding.OWUSiteID, binding.OWUAccountID, binding.CreatedByOperation,
		now.UnixNano()); err != nil {
		return domain.Binding{}, fmt.Errorf("install target allowlist entry: %w", err)
	}
	if err := replaceMappings(ctx, tx, binding.ID, binding.MessageMappings); err != nil {
		return domain.Binding{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO target_baselines(
			operation_id, binding_id, binding_version, source_snapshot_hash,
			snapshot_hash, conflict_hash, snapshot_json, verified_at_ns
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?)`,
		verified.OperationID, binding.ID, sourceHash, targetHash,
		verified.VerifiedConflictHash, targetJSON, now.UnixNano()); err != nil {
		return domain.Binding{}, fmt.Errorf("persist verified target baseline: %w", err)
	}
	receipt := operation.Operation.RemoteReceiptID
	if receipt == "" {
		receipt = verified.RemoteReceiptID
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE operations
		SET status = 'succeeded', binding_id = ?, needs_readback = 0, remote_receipt_id = ?,
		    verified_at_ns = ?, updated_at_ns = ?
		WHERE operation_id = ? AND status = 'verifying'`,
		binding.ID, receipt, now.UnixNano(), now.UnixNano(), verified.OperationID); err != nil {
		return domain.Binding{}, fmt.Errorf("complete verified create operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.Binding{}, fmt.Errorf("commit verified create: %w", err)
	}
	rollback = false
	return binding, nil
}

func (s *Store) CompleteVerifiedUpdate(ctx context.Context, operationID string,
	verified TargetBaseline, bindingVersion uint64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(verified.ConflictHash) == "" {
		return errors.New("verified target conflict hash is required")
	}
	if verified.Snapshot.OfflineSimulation {
		return errors.New("offline simulation cannot become a verified target baseline")
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin verified update completion: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()
	operation, err := getOperation(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if operation.Kind != OperationUpdate || operation.Operation.Status != domain.OperationVerifying {
		return ErrInvalidTransition
	}
	if operation.Operation.BindingID == "" ||
		(verified.BindingID != "" && verified.BindingID != operation.Operation.BindingID) {
		return ErrUntrustedTarget
	}
	if err := validateAllowedBinding(ctx, tx, s.installation, operation.Operation.BindingID); err != nil {
		return err
	}
	var currentVersion uint64
	if err := tx.QueryRowContext(ctx,
		"SELECT version FROM bindings WHERE binding_id = ?", operation.Operation.BindingID,
	).Scan(&currentVersion); err != nil {
		return fmt.Errorf("read current binding version: %w", err)
	}
	if currentVersion != bindingVersion {
		return ErrBindingVersionChanged
	}
	plan, err := getPlan(ctx, tx, operation.PlanID)
	if err != nil {
		return err
	}
	if plan.BindingID != operation.Operation.BindingID || plan.Plan.BindingVersion != bindingVersion {
		return ErrBindingVersionChanged
	}
	if _, err := getSourceSnapshot(ctx, tx, plan.SourceSnapshotHash); err != nil {
		return err
	}
	verified.Snapshot.BusinessHash = ""
	targetHash, err := verified.Snapshot.ComputeHash()
	if err != nil {
		return err
	}
	verified.Snapshot.BusinessHash = targetHash
	targetJSON, err := json.Marshal(verified.Snapshot)
	if err != nil {
		return fmt.Errorf("marshal verified target: %w", err)
	}
	mappings := mappingsFromTarget(verified.Snapshot)
	source, err := getSourceSnapshot(ctx, tx, plan.SourceSnapshotHash)
	if err != nil {
		return err
	}
	if err := validateMappings(source, mappings); err != nil {
		return err
	}
	now := time.Now().UTC()
	newVersion := currentVersion + 1
	result, err := tx.ExecContext(ctx, `
		UPDATE bindings SET version = ?, last_source_snapshot_hash = ?, updated_at_ns = ?
		WHERE binding_id = ? AND version = ?`, newVersion, plan.SourceSnapshotHash,
		now.UnixNano(), operation.Operation.BindingID, currentVersion)
	if err != nil {
		return fmt.Errorf("advance binding version: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrBindingVersionChanged
	}
	if err := replaceMappings(ctx, tx, operation.Operation.BindingID, mappings); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO target_baselines(
			operation_id, binding_id, binding_version, source_snapshot_hash,
			snapshot_hash, conflict_hash, snapshot_json, verified_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, operationID, operation.Operation.BindingID,
		newVersion, plan.SourceSnapshotHash, targetHash, verified.ConflictHash,
		targetJSON, now.UnixNano()); err != nil {
		return fmt.Errorf("persist updated target baseline: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE operations SET status = 'succeeded', needs_readback = 0,
		       verified_at_ns = ?, updated_at_ns = ?
		WHERE operation_id = ? AND status = 'verifying'`,
		now.UnixNano(), now.UnixNano(), operationID); err != nil {
		return fmt.Errorf("complete verified update operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit verified update: %w", err)
	}
	rollback = false
	return nil
}

func (s *Store) GetBinding(ctx context.Context, id string) (domain.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return getBinding(ctx, s.conn, s.installation, id)
}

func (s *Store) GetBindingState(ctx context.Context, id string) (BindingState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, err := getBinding(ctx, s.conn, s.installation, id)
	if err != nil {
		return BindingState{}, err
	}
	var sourceHash string
	if err := s.conn.QueryRowContext(ctx,
		"SELECT last_source_snapshot_hash FROM bindings WHERE binding_id = ?", id,
	).Scan(&sourceHash); err != nil {
		return BindingState{}, fmt.Errorf("read binding source baseline: %w", err)
	}
	source, err := getSourceSnapshot(ctx, s.conn, sourceHash)
	if err != nil {
		return BindingState{}, err
	}
	baseline, err := getTargetBaseline(ctx, s.conn, id, binding.Version)
	if err != nil {
		return BindingState{}, err
	}
	return BindingState{Binding: binding, SourceSnapshot: source, TargetBaseline: baseline}, nil
}

func getBinding(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, installation Installation, id string) (domain.Binding, error) {
	if err := validateAllowedBinding(ctx, q, installation, id); err != nil {
		return domain.Binding{}, err
	}
	var binding domain.Binding
	var sourceIdentityJSON []byte
	var titlePolicy string
	err := q.QueryRowContext(ctx, `
		SELECT binding_id, installation_id, owner_id, owu_site_id, owu_account_id,
		       target_chat_id, created_by_operation, source_identity_json,
		       title_policy, version
		FROM bindings WHERE binding_id = ?`, id).Scan(
		&binding.ID, &binding.InstallationID, &binding.PrincipalID, &binding.OWUSiteID,
		&binding.OWUAccountID, &binding.TargetChatID, &binding.CreatedByOperation,
		&sourceIdentityJSON, &titlePolicy, &binding.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Binding{}, ErrNotFound
	}
	if err != nil {
		return domain.Binding{}, fmt.Errorf("read binding: %w", err)
	}
	if err := json.Unmarshal(sourceIdentityJSON, &binding.SourceIdentity); err != nil {
		return domain.Binding{}, fmt.Errorf("%w: decode source identity", ErrCorrupt)
	}
	binding.TitlePolicy = domain.TitlePolicy(titlePolicy)
	rows, err := q.QueryContext(ctx, `
		SELECT source_message_id, target_message_id FROM message_mappings
		WHERE binding_id = ? ORDER BY source_message_id`, id)
	if err != nil {
		return domain.Binding{}, fmt.Errorf("read message mappings: %w", err)
	}
	for rows.Next() {
		var mapping domain.MessageMapping
		if err := rows.Scan(&mapping.SourceMessageID, &mapping.TargetMessageID); err != nil {
			_ = rows.Close()
			return domain.Binding{}, fmt.Errorf("scan message mapping: %w", err)
		}
		binding.MessageMappings = append(binding.MessageMappings, mapping)
	}
	if err := rows.Close(); err != nil {
		return domain.Binding{}, err
	}
	if err := rows.Err(); err != nil {
		return domain.Binding{}, err
	}
	return binding, nil
}

type allowedBindingQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validateAllowedBinding(ctx context.Context, q allowedBindingQuerier, installation Installation, id string) error {
	var count int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM bindings b
		JOIN target_allowlist a
		  ON a.binding_id = b.binding_id AND a.target_chat_id = b.target_chat_id
		JOIN creation_intents ci ON ci.operation_id = b.created_by_operation
		JOIN operations o ON o.operation_id = ci.operation_id
		WHERE b.binding_id = ?
		  AND b.installation_id = ? AND b.owner_id = ?
		  AND b.owu_site_id = ? AND b.owu_account_id = ?
		  AND a.installation_id = b.installation_id AND a.owner_id = b.owner_id
		  AND a.owu_site_id = b.owu_site_id AND a.owu_account_id = b.owu_account_id
		  AND a.creation_operation_id = b.created_by_operation
		  AND o.owner_id = b.owner_id AND o.kind = 'create' AND o.status = 'succeeded'
		  AND o.binding_id = b.binding_id AND o.target_chat_id = b.target_chat_id
		  AND o.remote_receipt_id <> ''`,
		id, installation.ID, installation.OwnerID, installation.OWUSiteID,
		installation.OWUAccountID).Scan(&count)
	if err != nil {
		return fmt.Errorf("validate target allowlist: %w", err)
	}
	if count != 1 {
		return ErrUntrustedTarget
	}
	return nil
}

func getTargetBaseline(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, bindingID string, version uint64) (TargetBaseline, error) {
	var baseline TargetBaseline
	var payload []byte
	var verifiedNS int64
	err := q.QueryRowContext(ctx, `
		SELECT operation_id, source_snapshot_hash, snapshot_hash, conflict_hash,
		       snapshot_json, verified_at_ns
		FROM target_baselines WHERE binding_id = ? AND binding_version = ?`,
		bindingID, version).Scan(&baseline.OperationID, &baseline.SourceSnapshotHash,
		&baseline.SnapshotHash, &baseline.ConflictHash, &payload, &verifiedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return TargetBaseline{}, ErrNotFound
	}
	if err != nil {
		return TargetBaseline{}, fmt.Errorf("read target baseline: %w", err)
	}
	if err := json.Unmarshal(payload, &baseline.Snapshot); err != nil {
		return TargetBaseline{}, fmt.Errorf("%w: decode target baseline", ErrCorrupt)
	}
	computed, err := baseline.Snapshot.ComputeHash()
	if err != nil || computed != baseline.SnapshotHash || baseline.Snapshot.BusinessHash != baseline.SnapshotHash {
		return TargetBaseline{}, fmt.Errorf("%w: target baseline hash mismatch", ErrCorrupt)
	}
	baseline.BindingID = bindingID
	baseline.BindingVersion = version
	baseline.VerifiedAt = time.Unix(0, verifiedNS).UTC()
	return baseline, nil
}

func replaceMappings(ctx context.Context, tx *sql.Tx, bindingID string, mappings []domain.MessageMapping) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM message_mappings WHERE binding_id = ?", bindingID); err != nil {
		return fmt.Errorf("replace message mappings: %w", err)
	}
	for _, mapping := range mappings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_mappings(binding_id, source_message_id, target_message_id)
			VALUES (?, ?, ?)`, bindingID, mapping.SourceMessageID, mapping.TargetMessageID); err != nil {
			return fmt.Errorf("save message mapping: %w", err)
		}
	}
	return nil
}

func validateMappings(source domain.SourceSnapshot, mappings []domain.MessageMapping) error {
	known := make(map[string]struct{}, len(source.Messages))
	for _, message := range source.Messages {
		known[message.ID] = struct{}{}
	}
	sourceSeen := make(map[string]struct{}, len(mappings))
	targetSeen := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		if mapping.SourceMessageID == "" || mapping.TargetMessageID == "" {
			return errors.New("message mapping IDs cannot be empty")
		}
		if _, ok := known[mapping.SourceMessageID]; !ok {
			return errors.New("message mapping references an unknown source message")
		}
		if _, exists := sourceSeen[mapping.SourceMessageID]; exists {
			return errors.New("duplicate source message mapping")
		}
		if _, exists := targetSeen[mapping.TargetMessageID]; exists {
			return errors.New("duplicate target message mapping")
		}
		sourceSeen[mapping.SourceMessageID] = struct{}{}
		targetSeen[mapping.TargetMessageID] = struct{}{}
	}
	return nil
}

func mappingsFromTarget(target domain.TargetSnapshot) []domain.MessageMapping {
	result := make([]domain.MessageMapping, 0, len(target.Messages))
	for _, message := range target.Messages {
		if message.Managed && message.SourceID != "" && message.ID != "" {
			result = append(result, domain.MessageMapping{
				SourceMessageID: message.SourceID,
				TargetMessageID: message.ID,
			})
		}
	}
	return result
}

func sameMappings(left, right []domain.MessageMapping) bool {
	if len(left) != len(right) {
		return false
	}
	rightBySource := make(map[string]string, len(right))
	for _, mapping := range right {
		rightBySource[mapping.SourceMessageID] = mapping.TargetMessageID
	}
	for _, mapping := range left {
		if rightBySource[mapping.SourceMessageID] != mapping.TargetMessageID {
			return false
		}
	}
	return true
}
