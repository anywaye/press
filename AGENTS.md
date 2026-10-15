# press — the documentation service

One binary, no other runtime dependency. It receives documentation tarballs
from `cafe docs publish`, verifies the caller against identity, records the
ingest, and enqueues the build whose artifacts nginx serves from R2 under the
caller's tenant prefix.

## The rule this repository is built around

**The tenant is the token's `account_id`, never a request field.** The ingest
API has no tenant parameter; a caller that sends one is refused. Every object
key is derived from the verified account (`internal/store/keys.go`), and
every table row carries `tenant_id` with every statement filtering by it. The
isolation is a property of the layout, not of a policy someone has to
remember to apply.

## What exists, and what does not

**Exists:** the ingest path (verify → cap → digest → R2 write → receipt
record), the key layout, the receipt and build records, the lifecycle rule
(queued → running → succeeded/failed, never backward).

**Does not exist yet, and this list is the work queue, not an omission:**

- the build worker: an ephemeral sandbox that checks out `cafaye/docs` at a
  pinned engine ref, syncs the tenant's content, runs the two builds, and
  writes `tenants/<org>/dist/…` to R2;
- the reconciliation run: ingest objects with no receipt row, and receipt rows
  whose build never finished;
- the goose runner (`internal/migrate`) and its gate job;
- per-team RBAC (frontmatter `teams:` checked against the token's claims) —
  consciously deferred: org-level public/private is the boundary v1 ships.

## Working agreements

- Read `moon/PLAN.md` §1 and §3 — the house rules apply here too.
- **Never push to master from a worker packet**; the manager merges.
- Tests are behavior tests: name the behavior, drive the path with fakes,
  never write a test whose subject is the framework. A test that needs
  Postgres is written the way identity's are (`TEST_DATABASE_URL`, skipped
  without it) and never pretends to be an offline test when it is not.
- One Postgres cluster, one database per service. press connects to the press
  database and its role owns nothing else.
