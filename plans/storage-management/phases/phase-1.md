# Phase 1 — root/path ownership

Status: **implementation-local correction complete; independent verification and review pending**.
Source identity: reviewed base `34b85134e11931efc2faff972bcb2d1110154153` and
candidate `9accc19d11b15e9896191e2c963b8f449d928116`.

## Contract

Represent configured paths by cleaned lexical identity resolved against an explicit captured base; do not use `EvalSymlinks` or infer case-insensitivity from GOOS for that identity. Existing-object equality requires filesystem evidence (`os.Stat`/`os.SameFile`). Definitions and IDs remain independent of existence. Use immutable definitions for the captured configuration and fresh operation-scoped physical observations. Merge role restrictions only on proven equality; do not persist observations into configured identity.

**Protected-root invariant:** before the first removal, every intended effect of
a legacy whole-HF delete must be eligible against all configured protected roots
in the operation's current configuration snapshot. No effect may delete a
protected root or enclose its contents, whether the relationship is lexical or
physically aliased. This applies to both the selected Hub repository and its
secondary friendly subtree. A protected/unknown effect refuses the operation
with no hub or friendly removal. Snapshot freshness must not be bypassed by
reusing an old root set across requests.

Separate three propositions:

1. Configured-definition relation: lexical reservation/ownership, valid even for
   missing paths; it is not proof of existing physical ancestry.
2. Physical relation: equal, ancestor/descendant, proven outside, or unknown from
   fresh filesystem observations; failure to observe an ancestor is not proof
   of noncontainment.
3. Effect eligibility: exact selected-root shape, applicable role/confinement,
   destructive component policy and absence of protected-root intersection.

Choose resolved physical observations plus `Stat`/`SameFile` ancestor evidence
inside the existing domain. `EvalSymlinks` is permitted for observation, never
configured IDs or substituting a newly trusted destructive root. Resolve both
sides, validate their observed objects, and inspect resolved rather than alias
namespace ancestors. Case/volume/reparse spelling alone cannot prove outside.
Keep lexical ownership distinct from the result used to authorize mutations.

For a missing descendant, observe the deepest existing prefix and unresolved
suffix separately. A suffix beneath an existing alias into an effect must remain
protected, not disappear as `IsNotExist`. A verified ordinary absent suffix can
establish a nonintersecting reserved location; it does not acquire fictional
physical identity. Dangling links, loops, inaccessible/non-directory prefixes,
unverified ancestry or unsupported alias semantics remain unknown and refuse
mutation. Do not globally refuse safe deletes solely because an unrelated
optional configured path is absent.

Keep read-only browsing and destructive authority separate. Preserve representative list/details behavior and friendly orphan visibility. No public physical-copy selectors or transactional deletion changes in this phase.

The common owner must preflight the fixed legacy effect set, with distinct Hub
physical-copy and friendly-secondary roles. Friendly is not an independent copy;
do not call `WholeCopyAllowed` indiscriminately for it or bypass the owner with
recursive cleanup. The legacy association selects the existing secondary path;
matching owner/name or a friendly role is not proof that files are projection
links. Proven-link inventory and the durable delete engine remain phase 3 work.

An optional friendly effect may be omitted only after safe absence observation;
unknown is not absent. After successful preflight, preserve existing ordinary
execution-error reporting, including safe secondary cleanup failures. Preflight
is not atomic execution, rollback, a lease or a hostile-writer/TOCTOU guarantee.

## State/transition map

| State / transition | Required outcome |
|---|---|
| Definitions captured; fresh relation observations gathered | IDs unchanged; no physical fact inferred from configured spelling |
| Any effect contains/equal-protects a configured root, or relevant relation is unknown | Refuse before any removal; retain both effects and protected sentinels |
| Hub allowed; friendly blocked | Refuse before Hub removal, not a post-Hub partial success |
| Friendly safely absent; Hub and remaining boundaries allowed | No secondary removal; retain normal legacy delete behavior |
| All intended effects allowed | Execute existing legacy unit; no new delete API or engine |
| Ordinary cleanup I/O failure after allowed Hub removal | Preserve existing visible secondary-failure response; no rollback claim |
| Next request/config generation or filesystem observation changes | Re-capture/re-observe; never turn stale evidence into permission |

## Case-to-verification map

| Case | Verification |
|---|---|
| C1: External alias points to existing child inside real Hub target (F1) | Domain `NestedProtectedRoots`/eligibility plus real handler refusal; Hub, friendly and child sentinel all preserved |
| C2: External alias points to child inside friendly effect (F1 + F2); direct friendly nested root (F2) | Model and dataset API cases; refusal before either effect; both primary and secondary markers retained |
| C3: Protected alias equals effect; both sides aliased; aliases chain | Fresh physical equality/ancestry tests; no destructive trust substitution |
| C4: Missing configured child below existing alias into effect | Domain reservation/unknown outcome plus API refusal; keep existing effect bytes and stable root ID |
| C5: Dangling/cyclic link, inaccessible prefix, invalid/unsupported reparse facts | Unknown/refusal without mutation; injected observation errors where native permission tests cannot establish denial |
| C6: Aliased parent with normal child, nested configured libraries, prefix sibling | Preserve owner selection, owned walker, scanner/finder/details counts/files/GGUF metadata; prefix sibling not enclosed |
| C7: Safe external aliases and unrelated ordinary missing roots; friendly absent | Normal deletion allowed where outside/absence is actually established; no overbroad all-alias/all-missing refusal |
| C8: Case-sensitive Linux/macOS, native Windows case/drive/junction facts | IDs remain lexical; facts govern existing relations; unsupported capabilities reported, not inferred by GOOS |
| C9: Normal model/dataset, default and exact external H, ordinary friendly subtree | Preserve successful legacy deletion, sibling/root sentinels, #95 ENV precedence and read-only metadata |
| C10: Allowed operation with safe secondary cleanup I/O failure | Preserve visible secondary-failure response; distinguish from preflight protection refusal |
| C11: Captured base/cwd, fresh observations and immutable config definitions | Preserve named captured-base tests, missing-to-created root ID and old/new snapshot tests |
| C12: Friendly source priority, raw cache restrictions, frozen job association | Preserve named priority tests, list/details, dedup/cleanup and lifecycle regression suites |
| C13: Alias browse versus symlink root/owner/repo mutation refusal | Read-only content detection still works; existing #95 external-H destructive guards stay fail-closed |
| C14: Manifest association evidence | Existing SameFile regression remains read-only evidence, not mutation authority |

## Implementation boundary

`pkg/hfdownloader.ManagedRootSet` remains the single storage-policy owner;
`internal/server/storage_roots.go` assembles configuration/display inputs and
adapts results. Correct the shared ancestry operation used by `OwnerForPath`,
`NestedProtectedRoots`, `WalkOwned` and `WholeCopyAllowed`; do not add a guard
per finding. Add/consolidate one non-mutating fixed legacy-HF effect preflight
in that owner and route both existing destructive sites through its decision.
The server derives actual effect paths from #95, calls the common preflight
before any removal, maps refusal and executes the existing operation.

Candidate implementation is not yet proven to satisfy this boundary: F1's
lexical-parent walk misses inverse aliases, and F2 guards only the primary Hub
effect. See [source trace and assignments](../reviews/protected-roots-reassessment.md).
No local unit/selective delete, public selector/If-Match, lease, journal,
quarantine, rollback, transaction or new product layout policy is authorized.

## Completion evidence

Establish failing C1/C2/C4 cases before correction. Run focused domain/server
regressions, relevant existing consumers, full tests/race, vet and build. Then
one independent tester checkpoint covers the integrated ancestry/effect-set
boundary; a full independent review covers the selected stable whole diff.
Bind evidence to the post-integration base/head and executing dirty state.
Native Windows/Go 1.24 and macOS capability gaps must be explicit. Passing the
old suite is not a pass for these missing semantic cases.

## Implementation-local correction (dirty candidate)

The implementation now resolves existing physical prefixes for fresh
containment observations while keeping configured IDs lexical, and preflights
the fixed Hub-plus-friendly legacy effect set through `ManagedRootSet` before
the handler's first `RemoveAll`. Symlink leaf ownership remains lexical for
discovery so a friendly alias does not transfer to its Hub target. Red/green
focused regressions cover inverse-alias/missing-child protection and real model
and dataset handler refusals with both trees' sentinels intact. Full local tests,
race, vet and build passed on Linux Go 1.26.7. See
[`phase-1-impl.md`](../implementation/phase-1-impl.md) for exact state and
limits. This is not independent tester/reviewer evidence; native Windows,
macOS, and Go 1.24 remain unverified.
