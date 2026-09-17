// Package storage provides the durable, single-process SQLite state store for
// synchronization. It intentionally has no API for inserting an arbitrary
// managed target: a binding can only be installed by FinalizeVerifiedCreate
// after a persisted create intent, recorded remote result, and verified
// readback.
package storage

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/ChenM0M/gpt-owu-bridge/internal/domain"
)

var (
	ErrAlreadyInitialized    = errors.New("storage is already initialized")
	ErrNotInitialized        = errors.New("storage is not initialized")
	ErrIdentityMismatch      = errors.New("configured OWU identity does not match the installation")
	ErrDatabaseInUse         = errors.New("database is already open by another process")
	ErrCorrupt               = errors.New("database integrity check failed")
	ErrInsecurePermissions   = errors.New("database or data directory permissions are too broad")
	ErrNotFound              = errors.New("storage record not found")
	ErrImmutableConflict     = errors.New("immutable record already exists with different content")
	ErrIdempotencyConflict   = errors.New("idempotency key was already used for a different payload")
	ErrSourceAlreadyClaimed  = errors.New("source alias already has a trusted creation intent")
	ErrInvalidTransition     = errors.New("invalid operation state transition")
	ErrUntrustedTarget       = errors.New("target is not backed by a verified creation record")
	ErrOperationInProgress   = errors.New("another unresolved operation blocks this binding")
	ErrPlanExpired           = errors.New("sync plan has expired")
	ErrConfirmationRequired  = errors.New("sync plan requires confirmation")
	ErrBindingVersionChanged = errors.New("binding version changed after the plan was created")
)

// TargetIdentity is obtained from the configured OWU site and an authenticated
// identity probe. It is fixed when Initialize succeeds and must match on every
// later Open.
type TargetIdentity struct {
	OWUSiteID    string
	OWUAccountID string
}

// Installation is generated locally. OwnerID is deliberately not supplied by
// a caller or model in the single-owner M2 deployment.
type Installation struct {
	ID           string    `json:"id"`
	OwnerID      string    `json:"owner_id"`
	OWUSiteID    string    `json:"owu_site_id"`
	OWUAccountID string    `json:"owu_account_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type StoredPlan struct {
	Plan                       domain.SyncPlan       `json:"plan"`
	OwnerID                    string                `json:"owner_id"`
	BindingID                  string                `json:"binding_id,omitempty"`
	SourceSnapshotHash         string                `json:"source_snapshot_hash"`
	PreviousSnapshotHash       string                `json:"previous_snapshot_hash,omitempty"`
	ExpectedTargetHash         string                `json:"expected_target_hash,omitempty"`
	ExpectedTargetConflictHash string                `json:"expected_target_conflict_hash,omitempty"`
	DesiredTarget              domain.TargetSnapshot `json:"desired_target"`
	DesiredConflictHash        string                `json:"desired_conflict_hash,omitempty"`
	DesiredRawChat             json.RawMessage       `json:"desired_raw_chat,omitempty"`
	DesiredRawEnvelope         json.RawMessage       `json:"desired_raw_envelope,omitempty"`
	DesiredArchived            bool                  `json:"desired_archived"`
	DesiredPinned              bool                  `json:"desired_pinned"`
	DesiredFolderID            string                `json:"desired_folder_id,omitempty"`
	PayloadHash                string                `json:"payload_hash"`
	ConfirmationRequired       bool                  `json:"confirmation_required"`
	ConfirmedAt                *time.Time            `json:"confirmed_at,omitempty"`
	CreatedAt                  time.Time             `json:"created_at"`
}

type OperationKind string

const (
	OperationCreate  OperationKind = "create"
	OperationUpdate  OperationKind = "update"
	OperationArchive OperationKind = "archive"
)

type OperationRecord struct {
	Operation    domain.Operation `json:"operation"`
	Kind         OperationKind    `json:"kind"`
	PlanID       string           `json:"plan_id"`
	PayloadHash  string           `json:"payload_hash"`
	TargetChatID string           `json:"target_chat_id,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
}

type CreateRequest struct {
	OwnerID            string
	OperationID        string
	IdempotencyKey     string
	PlanID             string
	PayloadHash        string
	SourceSnapshotHash string
}

type PrepareOperationRequest struct {
	OwnerID        string
	OperationID    string
	IdempotencyKey string
	Kind           OperationKind
	BindingID      string
	PlanID         string
	PayloadHash    string
}

type TransitionPatch struct {
	TargetChatID    string
	RemoteReceiptID string
	NeedsReadback   *bool
	ErrorCode       string
	VerifiedAt      *time.Time
}

// VerifiedCreate is accepted only for an existing trusted create intent whose
// candidate target was recorded while moving to verifying.
type VerifiedCreate struct {
	OperationID          string
	BindingID            string
	TargetChatID         string
	RemoteReceiptID      string
	SourceIdentity       domain.SourceIdentity
	MessageMappings      []domain.MessageMapping
	TitlePolicy          domain.TitlePolicy
	VerifiedTarget       domain.TargetSnapshot
	VerifiedConflictHash string
}

type TargetBaseline struct {
	BindingID          string                `json:"binding_id"`
	Snapshot           domain.TargetSnapshot `json:"snapshot"`
	SnapshotHash       string                `json:"snapshot_hash"`
	ConflictHash       string                `json:"conflict_hash"`
	SourceSnapshotHash string                `json:"source_snapshot_hash"`
	BindingVersion     uint64                `json:"binding_version"`
	OperationID        string                `json:"operation_id"`
	VerifiedAt         time.Time             `json:"verified_at"`
}

type BindingState struct {
	Binding        domain.Binding        `json:"binding"`
	SourceSnapshot domain.SourceSnapshot `json:"source_snapshot"`
	TargetBaseline TargetBaseline        `json:"target_baseline"`
}
