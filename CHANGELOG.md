# Changelog

## 2026-10-15 — the ingest half of press

**The tenant-isolation model, complete.** press ingests a documentation
tarball into the caller's own tenant prefix and nowhere else. The invariant:
the tenant is the verified token's `account_id` — the API has no tenant field
and refuses a caller that sends one, the object key is derived from the
verified account plus a server-generated receipt id, and every statement is
tenant-scoped. `internal/store/keys_test.go` tests the layout as a pure
function because a boundary tested through the S3 client is tested one bug
at a time.

**The ingest path, under behavior tests.** `internal/ingest`: verify the
caller against identity's JWKS, enforce the cap while reading (an over-cap
tarball is refused before anything is written), digest the exact bytes, write
to `tenants/<org>/ingest/<id>.tar.gz` on R2, and record the receipt. The
failure order is honest: the object is written first, so a receipt-table
refusal is a 500 naming the orphan the reconciliation run will sweep, not a
201 hiding it. A pantry ingest's project and version are validated at the
service seam — a receipt whose project can never produce a valid artifact key
is never recorded.

**The build lifecycle, with its one rule.** `internal/builds`: queued →
running → succeeded/failed, never backward, and the artifact prefix is fixed
at creation from the tenant and kind so no caller decides where artifacts
land after the fact.

**The S3 client, written rather than pulled.** press talks to R2's S3 API
with a stdlib SigV4 implementation: the AWS SDK is a megabyte of transitive
dependencies to make one PUT, and the fleet's discipline is that a dependency
is a decision about what the project carries.

**What is not here:** the build worker, the reconciliation run, the goose
runner. The model they extend is complete and tested; the next packets fill
them in. AGENTS.md keeps that list where a contributor will see it.
