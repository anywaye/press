# press — the documentation service

One binary, no other runtime dependency. It receives documentation tarballs
from `cafe docs publish`, verifies the caller against identity, records the
ingest, and enqueues the build whose artifacts nginx serves from R2 under the
caller's tenant prefix.

## The rule this repository is built around

**The tenant is the token's `account_id`, never a request field.** The ingest
API has no tenant parameter; a caller that sends one is refused. Every object
key is derived from the verified account (`internal/store/keys.go`), and
every table row carries `tenant_id`. What scopes a statement — and the
by-id reads and the fleet-wide claim that deliberately do not — is declared
statement by statement in `tenancy.yml`. The
isolation is a property of the layout, not of a policy someone has to
remember to apply.

## What exists, and what does not

**Exists:** the ingest path (verify → cap → digest → R2 write → receipt
record), the key layout, the receipt and build records, the lifecycle rule
(queued → running → succeeded/failed, never backward), and the worker that
drives a build end to end — fetch, extract into the engine's content tree,
checkout the engine at a pinned ref, build, upload under the build's own
prefix. Every failure lands the record on `failed` with its reason.

**Does not exist yet, and this list is the work queue, not an omission:**

- the production orchestration: the loop that claims queued builds, a k3s
  Job per build with the toolchains it needs, and the cron entry point;
- the reconciliation run: ingest objects with no receipt row, and receipt rows
  whose build never finished;
- the goose runner (`internal/migrate`) and its gate job;
- the reconciliation run: ingest objects with no receipt row, and receipt rows
  whose build never finished;
- the goose runner (`internal/migrate`) and its gate job;
- per-team RBAC (frontmatter `teams:` checked against the token's claims) —
  consciously deferred: org-level public/private is the boundary v1 ships.

## Working agreements

- The house rules apply here too: SSH remotes for the repositories cafaye owns, and tests first: write the test, watch it fail, then implement.
- **Never push to master from a worker packet**; the manager merges.
- Tests are behavior tests: name the behavior, drive the path with fakes,
  never write a test whose subject is the framework. A test that needs
  Postgres is written the way identity's are (`TEST_DATABASE_URL`, skipped
  without it) and never pretends to be an offline test when it is not.
- One Postgres cluster, one database per service. press connects to the press
  database and its role owns nothing else.

## Secrets
- No `.env` files with values. Use `cafe secret set/get/list` (dev) and `cafe dev` (injects). See `docs/secrets.md`.
