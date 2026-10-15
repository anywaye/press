# press

The cafaye documentation service. **`cafe docs publish` sends it a tarball;
press files the tarball, records the ingest, and enqueues the build whose
artifacts nginx serves.** BSL 1.1 (`Change License: Apache-2.0`, four years
out): anyone can self-host it, only cafaye runs it as a service.

**Status: ingest and build under test; the production orchestration is the
next packet.** The tenant-isolation model, the key layout, the receipt table
and the build lifecycle are complete; what is not in the tree is the worker
that runs cafaye/docs's build in an ephemeral sandbox and writes `dist/` under
the tenant's prefix. That gap is stated here rather than implied by an empty
function.

## The invariant everything rests on

**The tenant is the token's `account_id`. Nothing else in the request can
name one.** There is no tenant field in the ingest API, and a caller that
sends one is refused rather than ignored. The tarball's key is computed from
the verified account and a server-generated receipt id, so a caller cannot
aim its upload at another tenant's prefix, and every table row carries
`tenant_id` and every statement filters by it.

## Layout

| | |
| --- | --- |
| `internal/auth` | token verification against identity's JWKS; the only trust boundary |
| `internal/store` | the S3 client press speaks, and the **key layout** — `tenants/<org>/ingest/…`, `tenants/<org>/dist/…` |
| `internal/ingest` | the ingest path: verify, cap, digest, write, record; the HTTP handler |
| `internal/builds` | the build lifecycle (queued → running → succeeded/failed) and the worker that drives it |
| `internal/platform/db` | the pool to the press database on the one Postgres cluster |
| `migrations/` | goose SQL: `ingest_receipts`, `builds` |

## Running it

```sh
mise install
go build ./cmd/press
DATABASE_URL=postgres://press@localhost:5433/press \
PRESS_STORE_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
PRESS_STORE_BUCKET=cafaye-docs-artifacts \
PRESS_STORE_ACCESS_KEY_ID=… PRESS_STORE_SECRET_ACCESS_KEY=… \
./press
```

`DATABASE_URL` is required and points at the **press** database on the shared
cluster — press owns one database on it and its role owns nothing else
(architecture/one-cluster). The store is checked at readiness rather than at
boot, so a misconfigured bucket is a `/readyz` failure that says so.

## Tests

Behavior tests, not framework tests: each one names a thing press must do (or
must refuse) and drives the whole path — verifier to store to receipts — with
fakes. The boundary that matters is tested as a pure function
(`internal/store/keys_test.go`), because a key layout tested only through an
S3 client is tested one bug at a time.

```sh
go test ./...
```

The walk runs: the ingest writes under the caller's own prefix and nowhere
else; a request naming a tenant is refused; a project name that could escape
its prefix is refused before anything is written; an over-cap tarball is
refused mid-read; an unverified caller writes nothing; the receipt's digest
and size are the exact bytes sent; the lifecycle refuses backward moves.
