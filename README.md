# press

The cafaye documentation service. **`cafe docs publish` sends it a tarball;
press files the tarball, records the ingest, and enqueues the build whose
artifacts nginx serves.** BSL 1.1 (`Change License: Apache-2.0`, four years
out): anyone can self-host it, only cafaye runs it as a service.

**Status: the ingest path, the build worker, the orchestration spine, and the
gate are in the tree; the deployment is not.** What exists: the ingest path
(verify → cap → digest → R2 write → receipt record), the build lifecycle
(queued → running → succeeded/failed, never backward), the worker that drives
a build end to end — fetch, extract into the engine's content tree, check out
the engine at a pinned ref, build via `SubprocessRunner`, upload under the
build's own prefix — the goose runner (`internal/migrate`, `bin/migrate`,
`cmd/press-migrate`), the claim loop (concurrency-safe claim in
`internal/builds/store.go`, the poll loop in `internal/builds/claim.go`,
the runnable `cmd/press-claim`), and `cmd/press-gate`, which is deployed and
live in front of docs.cafaye.com/notes. What is honestly still missing: the
deployment — every serving binary exists but none runs on Contabo yet:
`cmd/press` serves the ingest API on `:8080` (`r.Mount("/v1/ingest",
ingestSvc.Handler())`), and `cmd/press-claim` and `cmd/press-migrate` are
built — but no image is pushed, nothing schedules them, and the press
database and R2 credentials are not wired; the reconciliation run (ingest objects with no
receipt row, receipt rows whose build never finished); and multi-tenant
gating (the gate admits one allowlist; per-team RBAC is consciously
deferred).

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
| `internal/ingest` | the ingest path: verify, cap, digest, write, record; the HTTP handler (a serving binary that listens on it is not in the tree yet) |
| `internal/builds` | the build lifecycle (queued → running → succeeded/failed), the pgx store with the concurrency-safe claim, the poll loop, and the worker: extractor, git engine, `runner.go`'s subprocess runner, uploader |
| `internal/migrate` | the goose runner: applies embedded `migrations/` and exits; never called from the serving path |
| `internal/verify` | the gate's door: introspection against identity, admit only the allowlist |
| `internal/platform/db` | the pool to the press database on the one Postgres cluster |
| `migrations/` | goose SQL: `ingest_receipts`, `builds` |
| `cmd/press-migrate` | the deploy step: apply `migrations/` to `DATABASE_URL` and exit; what the substrate's one-shot migrate Job runs |
| `cmd/press-claim` | the orchestration poller: claim queued builds, run each through the worker to a terminal state; exits only on SIGTERM |
| `cmd/press-gate` | the notes site's door: nginx `auth_request` subrequests `/verify`; live in front of docs.cafaye.com/notes |
| `bin/migrate` | the developer-facing entrypoint to the migrate package |

## Self-hosting

What you need: Go 1.26, a Postgres with a database for press, an R2/S3
bucket, and identity's JWKS URL (defaults to
`https://auth.cafaye.com/.well-known/jwks.json` via `PRESS_JWKS_URL`).
Migrate first — the schema is a deploy step, not boot logic:

```sh
mise install
DATABASE_URL=postgres://press@localhost:5433/press ./bin/migrate
go build ./cmd/press-migrate ./cmd/press-claim ./cmd/press-gate
```

What each binary takes from the environment: `press-migrate` needs only
`DATABASE_URL`. `press-claim` needs `DATABASE_URL`, the engine checkout
(`PRESS_ENGINE_REPO` and a pinned `PRESS_ENGINE_REF` — an unpinned engine
is refused), the object store (`PRESS_R2_ENDPOINT`, `PRESS_R2_BUCKET`,
`PRESS_R2_ACCESS_KEY_ID`, `PRESS_R2_SECRET_ACCESS_KEY`), and optionally
`PRESS_POLL_INTERVAL` (default 5s). `press-gate` needs `PRESS_IDENTITY_URL`
and `PRESS_NOTES_ACCOUNTS` (comma-separated; empty means a closed door),
listening on `PRESS_GATE_ADDR` (default `:8081`); wire your frontend's
`auth_request` at its `/verify` endpoint the way cafaye/docs
`deploy/docs.cafaye.com.yaml` wires nginx.

What you get, honestly: the migrate/claim/gate binaries run; the gate doors
a private site behind an allowlist. The ingest HTTP surface exists
(`internal/ingest`'s `Service.Handler`: verify, cap, digest, write, record)
but no serving binary listens on it yet, so a self-hoster takes the
`cafe docs publish` path by wiring `Handler` to a port themselves. There is
no reconciliation run — orphaned objects and unfinished builds are yours to
sweep — and no per-team gating: the door admits one allowlist per tenant.

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

## The gate (cmd/press-gate)

The private notes site's door, **deployed and live** in front of
docs.cafaye.com/notes: it runs on the Contabo k3s cluster, where nginx's
`auth_request` subrequests its `/verify` endpoint before serving any notes
request (manifest: cafaye/docs `deploy/docs.cafaye.com.yaml`).
It resolves the caller through identity's introspection route and admits only
`PRESS_NOTES_ACCOUNTS` — today the operator account that owns the notes;
press's tenant model is where per-org gating arrives. Failures are closed: an
unreachable identity, an empty allowlist, a token for another org — all 401,
none distinguishable from a forged token.
