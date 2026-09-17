CREATE TABLE installations (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    installation_id TEXT NOT NULL UNIQUE,
    owner_id TEXT NOT NULL UNIQUE,
    owu_site_id TEXT NOT NULL,
    owu_account_id TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE source_snapshots (
    business_hash TEXT PRIMARY KEY,
    payload_json BLOB NOT NULL,
    created_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE sync_plans (
    plan_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    binding_id TEXT NOT NULL DEFAULT '',
    source_snapshot_hash TEXT NOT NULL REFERENCES source_snapshots(business_hash),
    previous_snapshot_hash TEXT NOT NULL DEFAULT '',
    expected_target_hash TEXT NOT NULL DEFAULT '',
    expected_target_conflict_hash TEXT NOT NULL DEFAULT '',
    desired_target_json BLOB NOT NULL,
    desired_conflict_hash TEXT NOT NULL DEFAULT '',
	 desired_raw_chat BLOB NOT NULL,
	 desired_raw_envelope BLOB NOT NULL,
	 desired_archived INTEGER NOT NULL CHECK (desired_archived IN (0, 1)),
	 desired_pinned INTEGER NOT NULL CHECK (desired_pinned IN (0, 1)),
	 desired_folder_id TEXT NOT NULL DEFAULT '',
    plan_json BLOB NOT NULL,
    payload_hash TEXT NOT NULL,
    binding_version INTEGER NOT NULL,
    confirmation_required INTEGER NOT NULL CHECK (confirmation_required IN (0, 1)),
    confirmed_at_ns INTEGER,
    created_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE operations (
    operation_id TEXT PRIMARY KEY,
    owner_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK (kind IN ('create', 'update', 'archive')),
    binding_id TEXT NOT NULL DEFAULT '',
    lock_key TEXT NOT NULL,
    plan_id TEXT NOT NULL REFERENCES sync_plans(plan_id),
    payload_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('prepared', 'applying', 'verifying', 'succeeded', 'failed', 'needs_reconciliation')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    needs_readback INTEGER NOT NULL DEFAULT 0 CHECK (needs_readback IN (0, 1)),
    last_error_code TEXT NOT NULL DEFAULT '',
    remote_receipt_id TEXT NOT NULL DEFAULT '',
    target_chat_id TEXT NOT NULL DEFAULT '',
    verified_at_ns INTEGER,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX one_unresolved_operation_per_lock
ON operations(lock_key)
WHERE status IN ('prepared', 'applying', 'verifying', 'needs_reconciliation');

CREATE TABLE creation_intents (
    operation_id TEXT PRIMARY KEY REFERENCES operations(operation_id),
    source_snapshot_hash TEXT NOT NULL REFERENCES source_snapshots(business_hash),
	 source_alias TEXT NOT NULL UNIQUE,
    created_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE bindings (
    binding_id TEXT PRIMARY KEY,
    installation_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    owu_site_id TEXT NOT NULL,
    owu_account_id TEXT NOT NULL,
    target_chat_id TEXT NOT NULL UNIQUE,
    created_by_operation TEXT NOT NULL UNIQUE REFERENCES creation_intents(operation_id),
    source_identity_json BLOB NOT NULL,
    title_policy TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    last_source_snapshot_hash TEXT NOT NULL REFERENCES source_snapshots(business_hash),
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    FOREIGN KEY (installation_id) REFERENCES installations(installation_id),
    FOREIGN KEY (owner_id) REFERENCES installations(owner_id)
) STRICT;

CREATE TABLE target_allowlist (
    target_chat_id TEXT PRIMARY KEY,
    binding_id TEXT NOT NULL UNIQUE REFERENCES bindings(binding_id),
    installation_id TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    owu_site_id TEXT NOT NULL,
    owu_account_id TEXT NOT NULL,
    creation_operation_id TEXT NOT NULL UNIQUE REFERENCES creation_intents(operation_id),
    verified_at_ns INTEGER NOT NULL,
    FOREIGN KEY (installation_id) REFERENCES installations(installation_id),
    FOREIGN KEY (owner_id) REFERENCES installations(owner_id)
) STRICT;

CREATE TABLE message_mappings (
    binding_id TEXT NOT NULL REFERENCES bindings(binding_id) ON DELETE RESTRICT,
    source_message_id TEXT NOT NULL,
    target_message_id TEXT NOT NULL,
    PRIMARY KEY (binding_id, source_message_id),
    UNIQUE (binding_id, target_message_id)
) STRICT;

CREATE TABLE target_baselines (
    operation_id TEXT PRIMARY KEY REFERENCES operations(operation_id),
    binding_id TEXT NOT NULL REFERENCES bindings(binding_id),
    binding_version INTEGER NOT NULL,
    source_snapshot_hash TEXT NOT NULL REFERENCES source_snapshots(business_hash),
    snapshot_hash TEXT NOT NULL,
    conflict_hash TEXT NOT NULL,
    snapshot_json BLOB NOT NULL,
    verified_at_ns INTEGER NOT NULL,
    UNIQUE (binding_id, binding_version)
) STRICT;

CREATE INDEX target_baselines_binding_version
ON target_baselines(binding_id, binding_version DESC);
