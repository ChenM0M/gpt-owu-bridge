// Package domain defines the transport- and storage-independent contracts used
// by the bridge. The M0/M1 implementation only constructs read-only plans; the
// same types intentionally leave room for persisted operations in later stages.
package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const (
	SourceChatGPTShare = "chatgpt_share"
	ParserVersion      = "chatgpt-share-ref-v2"
	PolicyVersion      = "preview-policy-v2"
)

type EvidenceLevel string

const (
	EvidenceUnknown   EvidenceLevel = "unknown"
	EvidenceCandidate EvidenceLevel = "candidate"
	EvidenceVerified  EvidenceLevel = "verified"
)

type SourceIdentity struct {
	CandidateID string        `json:"candidate_id,omitempty"`
	Evidence    EvidenceLevel `json:"evidence"`
	Basis       string        `json:"basis,omitempty"`
}

type UnsupportedItem struct {
	NodeID string `json:"node_id,omitempty"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// ContentDiagnostic explains how one source node was handled. Ignored and
// degraded nodes are non-blocking; only blocked diagnostics represent lost
// user-visible semantics and therefore make coverage partial.
type ContentDiagnostic struct {
	SourceID    string `json:"source_id"`
	NodeID      string `json:"node_id,omitempty"`
	Role        string `json:"role,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Disposition string `json:"disposition"`
	Detail      string `json:"detail"`
}

type Coverage struct {
	Status                  string              `json:"status"`
	SelectedMessages        int                 `json:"selected_messages"`
	ExcludedInternalNodes   int                 `json:"excluded_internal_nodes"`
	ExcludedNonMessageNodes int                 `json:"excluded_non_message_nodes"`
	Unsupported             []UnsupportedItem   `json:"unsupported,omitempty"`
	Diagnostics             []ContentDiagnostic `json:"diagnostics,omitempty"`
	Limitations             []string            `json:"limitations,omitempty"`
}

type Message struct {
	ID              string     `json:"id"`
	NodeID          string     `json:"node_id"`
	Role            string     `json:"role"`
	Channel         string     `json:"channel,omitempty"`
	Parts           []string   `json:"parts"`
	ParentNodeID    string     `json:"parent_node_id,omitempty"`
	ChildNodeIDs    []string   `json:"child_node_ids,omitempty"`
	Order           int        `json:"order"`
	SourceCreatedAt *time.Time `json:"source_created_at,omitempty"`
}

type SourceSnapshot struct {
	SourceType      string         `json:"source_type"`
	Identity        SourceIdentity `json:"source_identity"`
	ShareID         string         `json:"share_id,omitempty"`
	Title           string         `json:"title,omitempty"`
	Messages        []Message      `json:"messages"`
	CurrentNodeID   string         `json:"current_node_id"`
	SourceCreatedAt *time.Time     `json:"source_created_at,omitempty"`
	FetchedAt       time.Time      `json:"fetched_at"`
	ParserVersion   string         `json:"parser_version"`
	Coverage        Coverage       `json:"coverage"`
	BusinessHash    string         `json:"business_hash"`
}

// ComputeHash excludes fetch time and the hash field itself. It is therefore a
// business snapshot hash, not a hash of the untrusted HTML container.
func (s SourceSnapshot) ComputeHash() (string, error) {
	copy := s
	copy.FetchedAt = time.Time{}
	copy.BusinessHash = ""
	// Diagnostics and exclusion counters describe the source container, not the
	// user-visible transcript that will be synchronized. Keeping them out of the
	// business hash prevents harmless tool/reasoning churn from looking like a
	// semantic conversation change. Status and Unsupported remain hashed so a
	// newly blocked visible message still changes the immutable plan input.
	copy.Coverage.Diagnostics = nil
	copy.Coverage.ExcludedInternalNodes = 0
	copy.Coverage.ExcludedNonMessageNodes = 0
	b, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal source snapshot: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type TitlePolicy string

const (
	TitleFollowSource   TitlePolicy = "follow_source"
	TitleTargetOverride TitlePolicy = "target_override"
)

type MessageMapping struct {
	SourceMessageID string `json:"source_message_id"`
	TargetMessageID string `json:"target_message_id"`
}

type Binding struct {
	ID                 string           `json:"id"`
	InstallationID     string           `json:"installation_id"`
	PrincipalID        string           `json:"principal_id"`
	OWUSiteID          string           `json:"owu_site_id"`
	OWUAccountID       string           `json:"owu_account_id"`
	TargetChatID       string           `json:"target_chat_id"`
	CreatedByOperation string           `json:"created_by_operation"`
	SourceIdentity     SourceIdentity   `json:"source_identity"`
	MessageMappings    []MessageMapping `json:"message_mappings"`
	TitlePolicy        TitlePolicy      `json:"title_policy"`
	Version            uint64           `json:"version"`
}

type TargetMessage struct {
	ID       string   `json:"id"`
	SourceID string   `json:"source_id,omitempty"`
	Role     string   `json:"role"`
	Channel  string   `json:"channel,omitempty"`
	Parts    []string `json:"parts"`
	Managed  bool     `json:"managed"`
	Order    int      `json:"order"`
}

type TargetSnapshot struct {
	OfflineSimulation bool            `json:"offline_simulation"`
	Title             string          `json:"title"`
	Messages          []TargetMessage `json:"messages"`
	CurrentMessageID  string          `json:"current_message_id,omitempty"`
	BusinessHash      string          `json:"business_hash,omitempty"`
}

func (s TargetSnapshot) ComputeHash() (string, error) {
	copy := s
	copy.BusinessHash = ""
	b, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal target snapshot: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type MessageChange struct {
	SourceID string `json:"source_id"`
	Kind     string `json:"kind"`
	Detail   string `json:"detail,omitempty"`
}

type Diff struct {
	Added           []MessageChange `json:"added,omitempty"`
	Modified        []MessageChange `json:"modified,omitempty"`
	ScopeReduced    []MessageChange `json:"scope_reduced,omitempty"`
	TargetConflicts []MessageChange `json:"target_conflicts,omitempty"`
	TitleChange     string          `json:"title_change"`
	PreviousTitle   string          `json:"previous_title,omitempty"`
	SourceTitle     string          `json:"source_title,omitempty"`
	TargetTitle     string          `json:"target_title,omitempty"`
}

type SyncPlan struct {
	ID                 string    `json:"id"`
	ReadOnly           bool      `json:"read_only"`
	Status             string    `json:"status"`
	SourceSnapshotHash string    `json:"source_snapshot_hash"`
	BindingVersion     uint64    `json:"binding_version"`
	TargetBaselineHash string    `json:"target_baseline_hash,omitempty"`
	ParserVersion      string    `json:"parser_version"`
	PolicyVersion      string    `json:"policy_version"`
	Diff               Diff      `json:"diff"`
	AllowedActions     []string  `json:"allowed_actions"`
	NextAction         string    `json:"next_action"`
	ExpiresAt          time.Time `json:"expires_at"`
}

type OperationStatus string

const (
	OperationPrepared            OperationStatus = "prepared"
	OperationApplying            OperationStatus = "applying"
	OperationVerifying           OperationStatus = "verifying"
	OperationSucceeded           OperationStatus = "succeeded"
	OperationFailed              OperationStatus = "failed"
	OperationNeedsReconciliation OperationStatus = "needs_reconciliation"
)

type Operation struct {
	ID              string          `json:"id"`
	IdempotencyKey  string          `json:"idempotency_key"`
	BindingID       string          `json:"binding_id"`
	Status          OperationStatus `json:"status"`
	AttemptCount    int             `json:"attempt_count"`
	NeedsReadback   bool            `json:"needs_readback"`
	LastErrorCode   string          `json:"last_error_code,omitempty"`
	RemoteReceiptID string          `json:"remote_receipt_id,omitempty"`
	VerifiedAt      *time.Time      `json:"verified_at,omitempty"`
}

type ResultError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	NextAction  string `json:"next_action"`
	Retryable   bool   `json:"retryable"`
	OperationID string `json:"operation_id,omitempty"`
}

func (e *ResultError) Error() string {
	return e.Code + ": " + e.Message
}

type SourceReader interface {
	Read(ctx context.Context, source string) (SourceSnapshot, error)
}

type TargetChatStore interface {
	Read(ctx context.Context, binding Binding) (TargetSnapshot, error)
	Create(ctx context.Context, operation Operation, snapshot SourceSnapshot) (string, error)
	Update(ctx context.Context, binding Binding, operation Operation, plan SyncPlan) error
	SetArchived(ctx context.Context, binding Binding, archived bool) error
}

type Repository interface {
	GetBinding(ctx context.Context, principalID, bindingID string) (Binding, error)
	SavePlan(ctx context.Context, principalID string, plan SyncPlan) error
	SaveOperation(ctx context.Context, principalID string, operation Operation) error
}
