

CREATE TABLE IF NOT EXISTS ingest_receipts (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    kind         text NOT NULL,
    project      text NOT NULL DEFAULT '',
    version      text NOT NULL DEFAULT '',
    sha256       text NOT NULL,
    size         bigint NOT NULL,
    state        text NOT NULL,
    object_key   text NOT NULL,
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
    artifact_prefix  text NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS builds_tenant_state
    ON builds (tenant_id, state);


DROP TABLE IF EXISTS builds;
DROP TABLE IF EXISTS ingest_receipts;
