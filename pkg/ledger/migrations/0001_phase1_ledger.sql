-- Herdforge Phase 1 private/local-operator ledger.
-- This migration owns only the herdforge schema. Cauldron is a separate
-- logical schema with a separate migration stream and must not be created here.

CREATE SCHEMA IF NOT EXISTS herdforge;

CREATE TABLE IF NOT EXISTS herdforge.actors (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    kind TEXT NOT NULL CHECK (kind IN ('operator', 'agent', 'service', 'system')),
    display_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS herdforge.principals (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    actor_id UUID NOT NULL REFERENCES herdforge.actors(id),
    kind TEXT NOT NULL CHECK (kind IN ('local_operator', 'local_service')),
    label TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (actor_id, kind, label)
);

CREATE TABLE IF NOT EXISTS herdforge.runs (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    repository TEXT NOT NULL,
    base_sha TEXT NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    owner_principal_id UUID NOT NULL REFERENCES herdforge.principals(id),
    status TEXT NOT NULL CHECK (status IN ('created', 'running', 'completed', 'failed', 'cancelled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS herdforge.phases (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    run_id UUID NOT NULL REFERENCES herdforge.runs(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    kind TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'passed', 'failed', 'blocked', 'skipped')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (run_id, ordinal)
);

CREATE TABLE IF NOT EXISTS herdforge.candidates (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    run_id UUID NOT NULL REFERENCES herdforge.runs(id),
    phase_id UUID REFERENCES herdforge.phases(id),
    git_sha TEXT NOT NULL CHECK (git_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    base_sha TEXT NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    evidence_digest TEXT NOT NULL CHECK (evidence_digest ~ '^sha256:[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (run_id, git_sha)
);

CREATE TABLE IF NOT EXISTS herdforge.receipts (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    candidate_id UUID NOT NULL REFERENCES herdforge.candidates(id),
    kind TEXT NOT NULL,
    evidence_digest TEXT NOT NULL CHECK (evidence_digest ~ '^sha256:[0-9a-f]{64}$'),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (candidate_id, kind, evidence_digest)
);

CREATE TABLE IF NOT EXISTS herdforge.reviews (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    candidate_id UUID NOT NULL REFERENCES herdforge.candidates(id),
    reviewer_principal_id UUID NOT NULL REFERENCES herdforge.principals(id),
    outcome TEXT NOT NULL CHECK (outcome IN ('pass', 'fail', 'blocked')),
    receipt_id UUID NOT NULL REFERENCES herdforge.receipts(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS herdforge.approvals (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    candidate_id UUID NOT NULL REFERENCES herdforge.candidates(id),
    approver_principal_id UUID NOT NULL REFERENCES herdforge.principals(id),
    decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
    receipt_id UUID NOT NULL REFERENCES herdforge.receipts(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS herdforge.spend_entries (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    run_id UUID NOT NULL REFERENCES herdforge.runs(id),
    actor_id UUID NOT NULL REFERENCES herdforge.actors(id),
    principal_id UUID NOT NULL REFERENCES herdforge.principals(id),
    amount_usd NUMERIC(18, 6) NOT NULL CHECK (amount_usd >= 0),
    token_count BIGINT NOT NULL DEFAULT 0 CHECK (token_count >= 0),
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS herdforge.owned_worktrees (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    run_id UUID NOT NULL REFERENCES herdforge.runs(id),
    candidate_id UUID REFERENCES herdforge.candidates(id),
    worktree_path TEXT NOT NULL CHECK (left(worktree_path, 2) = './'),
    branch TEXT NOT NULL,
    base_sha TEXT NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    owner_principal_id UUID NOT NULL REFERENCES herdforge.principals(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    released_at TIMESTAMPTZ,
    UNIQUE (run_id, worktree_path)
);

CREATE TABLE IF NOT EXISTS herdforge.lifecycle_events (
    id UUID PRIMARY KEY,
    contract_version SMALLINT NOT NULL DEFAULT 1 CHECK (contract_version = 1),
    run_id UUID NOT NULL REFERENCES herdforge.runs(id),
    sequence BIGINT NOT NULL CHECK (sequence > 0),
    event_type TEXT NOT NULL,
    actor_id UUID NOT NULL REFERENCES herdforge.actors(id),
    principal_id UUID NOT NULL REFERENCES herdforge.principals(id),
    phase_id UUID REFERENCES herdforge.phases(id),
    candidate_id UUID REFERENCES herdforge.candidates(id),
    evidence_digest TEXT CHECK (evidence_digest IS NULL OR evidence_digest ~ '^sha256:[0-9a-f]{64}$'),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    idempotency_key TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (run_id, sequence)
);

CREATE OR REPLACE FUNCTION herdforge.reject_candidate_identity_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.git_sha IS DISTINCT FROM OLD.git_sha
       OR NEW.base_sha IS DISTINCT FROM OLD.base_sha
       OR NEW.evidence_digest IS DISTINCT FROM OLD.evidence_digest THEN
        RAISE EXCEPTION 'candidate git SHA, base SHA, and evidence digest are immutable';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS candidates_identity_immutable ON herdforge.candidates;
CREATE TRIGGER candidates_identity_immutable
BEFORE UPDATE OF git_sha, base_sha, evidence_digest ON herdforge.candidates
FOR EACH ROW EXECUTE FUNCTION herdforge.reject_candidate_identity_change();

CREATE OR REPLACE FUNCTION herdforge.reject_lifecycle_event_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'lifecycle events are append-only';
END;
$$;

DROP TRIGGER IF EXISTS lifecycle_events_append_only ON herdforge.lifecycle_events;
CREATE TRIGGER lifecycle_events_append_only
BEFORE UPDATE OR DELETE ON herdforge.lifecycle_events
FOR EACH ROW EXECUTE FUNCTION herdforge.reject_lifecycle_event_mutation();
