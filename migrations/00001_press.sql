-- 00001_press.sql — press's schema: the ingest receipts and the builds.
--
-- Two tables and two principles. First, every row carries tenant_id and
-- every statement in this service filters by it — the same row-level tenancy
-- identity and billing use on the one cluster, where the database is the
-- isolation boundary and the tenant is the isolation within it. Second, both
-- tables are records, not state stores: press never mutates a row's meaning,
-- only its lifecycle column, and the artifact location is derived from the
-- tenant and the kind rather than trusted from a caller.
--
-- The one Postgres convention (architecture/one-cluster): the press role owns
-- this database, is NOSUPERUSER, and cannot reach identity's or billing's.

-- +goose Up

CREATE TABLE IF NOT EXISTS ingest_receipts (
    -- Server-generated, never caller-supplied: it is half of the object's
    -- key, and a caller that chose it could aim its upload at another
    -- tenant's receipt path.
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    kind         text NOT NULL,
    project      text NOT NULL DEFAULT '',
    version      text NOT NULL DEFAULT '',
    sha256       text NOT NULL,
    size         bigint NOT NULL,
    state        text NOT NULL,
    -- The bucket-relative key the tarball landed at, recorded so the
    -- reconciliation run can find orphans without re-deriving the layout.
    object_key   text NOT NULL,
    -- The identity's subject id, for an audit trail without a join.
    caller       text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ingest_receipts_tenant_created
    ON ingest_receipts (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS builds (
    id               text PRIMARY KEY,
    tenant_id        text NOT NULL,
    kind             text NOT NULL,
    receipt_id       text NOT NULL REFERENCES ingest_receipts (id),
    state            text NOT NULL,
    -- Computed at creation from tenant and kind. The record carries it so
    -- no caller decides where artifacts land after the fact.
    artifact_prefix  text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS builds_tenant_state
    ON builds (tenant_id, state);

-- +goose Down

DROP TABLE IF EXISTS builds;
DROP TABLE IF EXISTS ingest_receipts;
