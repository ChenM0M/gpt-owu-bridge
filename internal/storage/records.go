package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

func (s *Store) PutSourceSnapshot(ctx context.Context, snapshot domain.SourceSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return putSourceSnapshot(ctx, s.conn, snapshot)
}

func putSourceSnapshot(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, snapshot domain.SourceSnapshot) error {
	computed, err := snapshot.ComputeHash()
	if err != nil {
		return err
	}
	if snapshot.BusinessHash == "" || snapshot.BusinessHash != computed {
		return errors.New("source snapshot business hash is missing or invalid")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal source snapshot: %w", err)
	}
	result, err := exec.ExecContext(ctx, `
		INSERT INTO source_snapshots(business_hash, payload_json, created_at_ns)
		VALUES (?, ?, ?)
		ON CONFLICT(business_hash) DO NOTHING`,
		snapshot.BusinessHash, payload, time.Now().UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("save source snapshot: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect source snapshot insert: %w", err)
	}
	if inserted == 1 {
		return nil
	}
	var existing []byte
	if err := exec.QueryRowContext(ctx,
		"SELECT payload_json FROM source_snapshots WHERE business_hash = ?", snapshot.BusinessHash,
	).Scan(&existing); err != nil {
		return fmt.Errorf("read existing source snapshot: %w", err)
	}
	var stored domain.SourceSnapshot
	if err := json.Unmarshal(existing, &stored); err != nil {
		return fmt.Errorf("%w: decode existing source snapshot", ErrCorrupt)
	}
	storedHash, err := stored.ComputeHash()
	if err != nil || storedHash != snapshot.BusinessHash || stored.BusinessHash != snapshot.BusinessHash {
		return fmt.Errorf("%w: existing source snapshot hash mismatch", ErrCorrupt)
	}
	// FetchedAt is intentionally excluded from the business hash. Preserve the
	// first immutable observation when the same source content is fetched again.
	return nil
}

func (s *Store) GetSourceSnapshot(ctx context.Context, hash string) (domain.SourceSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return getSourceSnapshot(ctx, s.conn, hash)
}

func getSourceSnapshot(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, hash string) (domain.SourceSnapshot, error) {
	var payload []byte
	if err := q.QueryRowContext(ctx,
		"SELECT payload_json FROM source_snapshots WHERE business_hash = ?", hash,
	).Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return domain.SourceSnapshot{}, ErrNotFound
	} else if err != nil {
		return domain.SourceSnapshot{}, fmt.Errorf("read source snapshot: %w", err)
	}
	var snapshot domain.SourceSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return domain.SourceSnapshot{}, fmt.Errorf("decode source snapshot: %w", err)
	}
	computed, err := snapshot.ComputeHash()
	if err != nil || computed != hash || snapshot.BusinessHash != hash {
		return domain.SourceSnapshot{}, fmt.Errorf("%w: source snapshot hash mismatch", ErrCorrupt)
	}
	return snapshot, nil
}

func (s *Store) SavePlan(ctx context.Context, plan StoredPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if plan.Plan.ID == "" || plan.SourceSnapshotHash == "" {
		return errors.New("plan ID and source snapshot hash are required")
	}
	if plan.OwnerID == "" {
		plan.OwnerID = s.installation.OwnerID
	}
	if plan.OwnerID != s.installation.OwnerID {
		return ErrIdentityMismatch
	}
	if plan.Plan.SourceSnapshotHash != plan.SourceSnapshotHash {
		return errors.New("stored plan source snapshot does not match domain plan")
	}
	if plan.Plan.ExpiresAt.IsZero() {
		return errors.New("plan expiry is required")
	}
	plan.Plan.ExpiresAt = plan.Plan.ExpiresAt.UTC()
	if plan.DesiredTarget.OfflineSimulation {
		return errors.New("offline simulation target cannot be persisted as an executable plan")
	}
	if len(plan.DesiredRawChat) == 0 || !json.Valid(plan.DesiredRawChat) {
		return errors.New("desired raw OWU chat must be valid JSON")
	}
	if len(plan.DesiredRawEnvelope) != 0 && !json.Valid(plan.DesiredRawEnvelope) {
		return errors.New("desired raw OWU envelope must be valid JSON when present")
	}
	if _, err := getSourceSnapshot(ctx, s.conn, plan.SourceSnapshotHash); err != nil {
		return fmt.Errorf("plan source snapshot: %w", err)
	}
	if plan.PreviousSnapshotHash != "" {
		if _, err := getSourceSnapshot(ctx, s.conn, plan.PreviousSnapshotHash); err != nil {
			return fmt.Errorf("plan previous snapshot: %w", err)
		}
	}
	// Adapters may carry their stronger full-graph conflict hash separately.
	// Always normalize the domain business hash from the domain snapshot here.
	plan.DesiredTarget.BusinessHash = ""
	desiredHash, err := plan.DesiredTarget.ComputeHash()
	if err != nil {
		return err
	}
	plan.DesiredTarget.BusinessHash = desiredHash
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = time.Now().UTC()
	} else {
		plan.CreatedAt = plan.CreatedAt.UTC()
	}
	plan.ConfirmedAt = nil
	computedPayloadHash, err := storedPlanPayloadHash(plan)
	if err != nil {
		return err
	}
	if plan.PayloadHash != "" && plan.PayloadHash != computedPayloadHash {
		return errors.New("plan payload hash is invalid")
	}
	plan.PayloadHash = computedPayloadHash
	planJSON, err := json.Marshal(plan.Plan)
	if err != nil {
		return fmt.Errorf("marshal sync plan: %w", err)
	}
	desiredJSON, err := json.Marshal(plan.DesiredTarget)
	if err != nil {
		return fmt.Errorf("marshal desired target: %w", err)
	}
	desiredRawEnvelope := []byte(plan.DesiredRawEnvelope)
	if desiredRawEnvelope == nil {
		desiredRawEnvelope = []byte{}
	}
	result, err := s.conn.ExecContext(ctx, `
		INSERT INTO sync_plans(
			plan_id, owner_id, binding_id, source_snapshot_hash, previous_snapshot_hash,
			expected_target_hash, expected_target_conflict_hash, desired_target_json,
			desired_conflict_hash, desired_raw_chat, desired_raw_envelope,
			desired_archived, desired_pinned,
			desired_folder_id, plan_json, payload_hash, binding_version,
			confirmation_required, confirmed_at_ns, created_at_ns, expires_at_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
		ON CONFLICT(plan_id) DO NOTHING`,
		plan.Plan.ID, plan.OwnerID, plan.BindingID, plan.SourceSnapshotHash,
		plan.PreviousSnapshotHash, plan.ExpectedTargetHash, plan.ExpectedTargetConflictHash,
		desiredJSON, plan.DesiredConflictHash, []byte(plan.DesiredRawChat), desiredRawEnvelope,
		boolInt(plan.DesiredArchived), boolInt(plan.DesiredPinned), plan.DesiredFolderID,
		planJSON, plan.PayloadHash,
		plan.Plan.BindingVersion, boolInt(plan.ConfirmationRequired),
		plan.CreatedAt.UnixNano(), plan.Plan.ExpiresAt.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("save sync plan: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect sync plan insert: %w", err)
	}
	if inserted == 1 {
		return nil
	}
	existing, err := getPlan(ctx, s.conn, plan.Plan.ID)
	if err != nil {
		return err
	}
	if existing.PayloadHash != plan.PayloadHash {
		return ErrImmutableConflict
	}
	return nil
}

func (s *Store) GetPlan(ctx context.Context, id string) (StoredPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return getPlan(ctx, s.conn, id)
}

func getPlan(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (StoredPlan, error) {
	var result StoredPlan
	var planJSON, desiredJSON, desiredRawChat, desiredRawEnvelope []byte
	var confirmationRequired int
	var desiredArchived, desiredPinned int
	var confirmedNS sql.NullInt64
	var createdNS, expiresNS int64
	var bindingVersion uint64
	err := q.QueryRowContext(ctx, `
		SELECT owner_id, binding_id, source_snapshot_hash, previous_snapshot_hash,
		       expected_target_hash, expected_target_conflict_hash, desired_target_json,
		       desired_conflict_hash, desired_raw_chat, desired_raw_envelope,
		       desired_archived, desired_pinned,
		       desired_folder_id, plan_json, payload_hash, binding_version,
		       confirmation_required, confirmed_at_ns, created_at_ns, expires_at_ns
		FROM sync_plans WHERE plan_id = ?`, id).Scan(
		&result.OwnerID, &result.BindingID, &result.SourceSnapshotHash,
		&result.PreviousSnapshotHash, &result.ExpectedTargetHash,
		&result.ExpectedTargetConflictHash, &desiredJSON, &result.DesiredConflictHash,
		&desiredRawChat, &desiredRawEnvelope, &desiredArchived, &desiredPinned, &result.DesiredFolderID,
		&planJSON, &result.PayloadHash, &bindingVersion, &confirmationRequired,
		&confirmedNS, &createdNS, &expiresNS)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredPlan{}, ErrNotFound
	}
	if err != nil {
		return StoredPlan{}, fmt.Errorf("read sync plan: %w", err)
	}
	if err := json.Unmarshal(planJSON, &result.Plan); err != nil {
		return StoredPlan{}, fmt.Errorf("%w: decode sync plan", ErrCorrupt)
	}
	if err := json.Unmarshal(desiredJSON, &result.DesiredTarget); err != nil {
		return StoredPlan{}, fmt.Errorf("%w: decode desired target", ErrCorrupt)
	}
	result.DesiredRawChat = append(json.RawMessage(nil), desiredRawChat...)
	result.DesiredRawEnvelope = append(json.RawMessage(nil), desiredRawEnvelope...)
	result.DesiredArchived = desiredArchived != 0
	result.DesiredPinned = desiredPinned != 0
	result.Plan.BindingVersion = bindingVersion
	result.Plan.ExpiresAt = time.Unix(0, expiresNS).UTC()
	result.ConfirmationRequired = confirmationRequired != 0
	result.CreatedAt = time.Unix(0, createdNS).UTC()
	if confirmedNS.Valid {
		confirmedAt := time.Unix(0, confirmedNS.Int64).UTC()
		result.ConfirmedAt = &confirmedAt
	}
	computed, err := storedPlanPayloadHash(result)
	if err != nil || computed != result.PayloadHash {
		return StoredPlan{}, fmt.Errorf("%w: sync plan payload hash mismatch", ErrCorrupt)
	}
	return result, nil
}

func (s *Store) ConfirmPlan(ctx context.Context, id string, now time.Time) (StoredPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	plan, err := getPlan(ctx, s.conn, id)
	if err != nil {
		return StoredPlan{}, err
	}
	if plan.OwnerID != s.installation.OwnerID {
		return StoredPlan{}, ErrIdentityMismatch
	}
	if !now.Before(plan.Plan.ExpiresAt) {
		return StoredPlan{}, ErrPlanExpired
	}
	if plan.BindingID != "" {
		var version uint64
		err := s.conn.QueryRowContext(ctx,
			"SELECT version FROM bindings WHERE binding_id = ? AND owner_id = ?",
			plan.BindingID, s.installation.OwnerID).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) {
			return StoredPlan{}, ErrNotFound
		}
		if err != nil {
			return StoredPlan{}, fmt.Errorf("check binding version: %w", err)
		}
		if version != plan.Plan.BindingVersion {
			return StoredPlan{}, ErrBindingVersionChanged
		}
	} else if plan.Plan.BindingVersion != 0 {
		return StoredPlan{}, ErrBindingVersionChanged
	}
	if !plan.ConfirmationRequired || plan.ConfirmedAt != nil {
		return plan, nil
	}
	if _, err := s.conn.ExecContext(ctx,
		"UPDATE sync_plans SET confirmed_at_ns = ? WHERE plan_id = ? AND confirmed_at_ns IS NULL",
		now.UnixNano(), id); err != nil {
		return StoredPlan{}, fmt.Errorf("confirm sync plan: %w", err)
	}
	return getPlan(ctx, s.conn, id)
}

func storedPlanPayloadHash(plan StoredPlan) (string, error) {
	copy := plan
	copy.PayloadHash = ""
	copy.ConfirmedAt = nil
	payload, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal plan payload: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sourceAlias(snapshot domain.SourceSnapshot) (string, error) {
	if strings.TrimSpace(snapshot.SourceType) == "" || strings.TrimSpace(snapshot.ShareID) == "" {
		return "", errors.New("source type and share ID are required for a trusted create intent")
	}
	return snapshot.SourceType + ":" + snapshot.ShareID, nil
}
