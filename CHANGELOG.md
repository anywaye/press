# Changelog

## 2026-10-16 (later) — the account boundary, declared, and the claims measured false are corrected

**`tenancy.yml` is press's declaration under core's tenancy contract.** It
names `tenant_id` as the key (the spelling both tables and every statement
use), the two declared entry points — `ingest-receipt` (the ingest route: the
tenant is bound into the receipt row from the verified token, never from a
request field) and `build-claim` (the worker: the claimed row's tenant is read
back and the receipt must match it before anything is fetched) — with the
three-way denial arms pointing at real assertion lines, and `rls` saying the
truth: no policy, no enable, no force. `tenancy-check .` reports 0 failures
and 2 warnings, both the honest residue of a SQL scanner reading a Go service,
and `tenancy-proof .` holds in both directions.

**The declaration's own first job was catching claims that were false.** The
migration header, the README invariant and the `internal/builds/store.go`
header all said or implied "every statement filters by `tenant_id`". Measured:
six statements touch the two tables and three of them — `builds.Get`,
`builds.Transition` and the receipt read inside `Claim` — reach a row by
primary key with no tenant predicate (none caller-reachable; the claim's
breadth is by design and the worker's tenant comparison is what bounds it).
The sentences now say that, and `tenancy.yml` names every one of the six with
its reason. Nothing about the behaviour changed; the descriptions did.

**The cross-tenant worker test could not fail for the reason it claimed.**
`TestRunBuildRefusesAReceiptFromAnotherTenant` seeded no tarball, so a fetch
failure produced the same zero counts as the tenant guard — the test was green
either way. It now seeds acme's tarball under the receipt's key, so the guard
is the only thing that can refuse, and the zero-counts assertion was verified
able to fail (guard removed → red). Two arms that were missing were added:
a receipt naming no tenant (the no-identity arm), and a valid credential whose
body names another tenant through the real ingest handler (the other-account
arm, verified able to fail with the refusal removed).

## 2026-10-16 (latest) — the repository is public; its face tells the truth

**Docs only: the README and this file now describe the tree as it is.**
The repository went public on 2026-10-16 — the BSL license promises
self-hosting, and a private repository cannot keep that promise — and the
README still described a tree from several packets ago: a production
orchestration "not in the tree" that is (the claim loop, `cmd/press-claim`),
a sandbox worker "not in the tree" that is (the worker with its git engine
and subprocess runner), and a `go build ./cmd/press` that builds nothing
because no such binary exists. All three are corrected, and the correction
is the content of this entry: nothing else changed.

The Status paragraph now states what exists and what is honestly still
missing — the deployment (every serving binary exists — `cmd/press` serves
ingest on :8080 — but none runs on Contabo; database and R2 credentials not
wired), the
reconciliation run, multi-tenant gating. The Layout table covers
`cmd/press-migrate`, `cmd/press-claim`, `cmd/press-gate`,
`internal/migrate`, and `internal/builds/runner.go`. The gate section says
it is live and where it runs (nginx `auth_request` on the Contabo k3s
cluster). And `Running it` — which documented a binary that does not exist —
is replaced by a `Self-hosting` section that documents only binaries that do,
with the environment each one actually reads.

## 2026-10-15 — the gate, live on docs.cafaye.com/notes

**`cmd/press-gate` is the private notes site's door, and it is deployed.**
One endpoint answering traefik-side... no — answering nginx's `auth_request`
subrequest: resolve the caller's bearer token through identity's
`POST /v1/introspections`, admit only accounts on this tenant's allowlist,
fail closed on an unreachable identity. The gate's failure modes are its
whole subject: a valid token for the wrong org gets the same 401 as a forged
one, so the door is not an account-existence oracle; an identity outage is a
closed door, never an open one; an empty allowlist is a closed door, logged
at startup.

The deployment lives in cafaye/docs `deploy/docs.cafaye.com.yaml`. The
middleware-with-traefik approach was tried first and failed silently — the
notes home served to the open internet — which is why the subrequest lives
in nginx's own config now.

Six behavior tests drive the door: admit, refuse each wrong-caller shape,
fail closed, closed on empty allowlist, multi-account.

# Changelog

## 2026-10-15 (later) — the build worker

**The worker turns a receipt into artifacts.** Claim a queued build, fetch the
tarball, extract it into the engine's content tree (`docs/` for the public
kind, `notes/` for the private one), check out the docs engine at a pinned
ref, build, upload under the build's own prefix, transition the record. Every
step is a collaborator behind an interface, and every failure — runner,
store, upload — lands the record on `failed` with the reason. A build stuck
in `running` is a build whose tenant silently never gets artifacts, so no
code path leaves it there.

**The upload target is the build record's prefix and nothing else** —
computed at creation from the build's tenant and kind. The one cross-tenant
shape the worker can be handed (a receipt from another tenant in the call) is
refused before the tarball is even fetched: no fetch, no build, no upload.

**The tarball is untrusted input.** A member with a `..` element is refused,
not rewritten — the tempting defense of `Clean`-ing the path silently
renames the archive's content, and an archive whose meaning changed under
extraction is worse than one that failed loudly.

**The worker pins the engine ref it built against**, so a behavior change in
the engine is diagnosable per build rather than a fleet-wide surprise.

The cafaye/docs engine gained the matching half in the same window:
`bin/sync-content --source <dir>` builds one tenant's content into the two
trees with no fleet beside it — the tenant build is the same build with one
tenant's content, not a weaker one.

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
