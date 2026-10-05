# Phase 1 — root/path ownership

Status: **F3 correction implemented locally; independent/native evidence pending**.
Source identity: base `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e` and
candidate `7dc62163e9ac5dffc5b126fcd18ce8ec7c212fee`; merge base equals base.
Parent reports current Linux C1-C14 production verification passed; full review
requires changes for F3. No native mount reproduction or universal proof passed.

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

Resolved names and ancestor matches remain useful positive evidence, but
**negative ancestor searches are not outside proof**: bind mounts preserve
directory objects while exposing them under unrelated namespace parents.
`EvalSymlinks` is permitted for observation, never configured IDs or destructive
trust substitution. More resolution, reverse searches, different device IDs
or GOOS heuristics do not repair this inference.

Chosen correction level: the existing owner must observe the directory objects
reachable by each intended recursive removal. Protection is evaluated against
that complete operation-specific reachability, not merely naming-tree ancestry.
Traverse actual directory edges (including ordinary mounted directories) under
the remover's no-follow/leaf semantics; do not use `WalkOwned`, which prunes
exactly the protected subtrees the preflight must detect. Compare freshly
observed protected roots and missing-path anchors with reached objects using
established identity facts. Existing lexical reservation guards remain useful
conservative blockers. No effect is authorized until its proof is complete.

This is an ephemeral, read-only proof under ManagedRootSet, not an inventory,
generic filesystem framework, persisted graph, new remover or phase-3 plan.
Go 1.24 `os.Root` relative observations can help bind reads to the selected
effect. It permits mount traversal and is not itself proof of disjointness;
there is no `Root.Rename`/`Root.RemoveAll`. Preserve prior effect confinement.

For a missing descendant, retain the deepest verified existing directory object
and ordinary missing suffix separately, including prefixes reached through bind
or symlink aliases. If removal reaches that anchor and affects the reserved
location, refuse: `S/next` below a bind-mounted child of T cannot vanish from
protection just because next is absent. Never invent a child inode. A safe
outside reservation requires complete nonintersection evidence, not path
spelling or failed ancestors. Preserve unrelated ordinary missing roots and
safe friendly absence when proven.

Incomplete enumeration, unreadable/failed identity facts, unsupported entry
semantics, cycles, cancellation or exhausted resource bounds are unknown and
refuse before any removal. Stream metadata; do not read model bytes. Compare
opened-object facts where needed to avoid negative equality caused by deferred
identity-load failure. Distinct namespace occurrences of one directory object
can expose different child mounts; global SameFile deduplication is not proof
that those edges were covered. Existing ancestor-owner exemptions must not hide
the protected namespace object itself when a mount re-exposes it inside an
effect. Do not blanket-refuse all mounts or silently skip them.

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
| Definitions captured; fresh relation/reachability observations gathered | IDs unchanged; no physical fact inferred from configured spelling |
| Any effect contains/equal-protects a configured root, or relevant relation is unknown | Refuse before any removal; retain both effects and protected sentinels |
| Hub allowed; friendly blocked | Refuse before Hub removal, not a post-Hub partial success |
| Friendly safely absent; Hub and remaining boundaries allowed | No secondary removal; retain normal legacy delete behavior |
| All intended effects allowed | Execute existing legacy unit; no new delete API or engine |
| Ancestors differ but reached directory matches protected object/anchor | Refuse; namespace spelling cannot override the object match |
| Enumeration incomplete, active cycle or resource/capability failure | Unknown/refusal; never allow using a partial reached set |
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
| C15: External bind S exposes existing child C inside Hub T | Domain eligibility and nested protection recognize the reached protected object; API refusal keeps Hub, friendly and sentinel |
| C16: External bind S exposes child inside friendly effect | Complete preflight refuses before Hub removal; model/dataset trees both retained |
| C17: Missing protected reservation S/next with bind-mounted anchor C | Anchor/suffix retained, unknown/intersection refuses; no fictional inode or dropped definition |
| C18: Mount inside effect exposes protected root outside its naming tree | Traverse actual removal edges; detect protected object before mutation; EBUSY at final rmdir is not safety proof |
| C19: Mount re-exposes authorizing Hub/friendly namespace inside an effect | Do not exempt the reached protected object solely by owner root ID; preserve ordinary ancestor authorization |
| C20: Repeated directory objects with different child mount views; active cycle | Visit relevant namespace occurrences; no unsound global-object pruning; cycle/incomplete proof refuses |
| C21: Plain symlink/reparse leaf versus traversed mounted directory | Match actual Go 1.24 remover semantics; do not follow symlink content or assume junction equals Linux bind behavior |
| C22: Incomplete ReadDir/stat/object identity; proven unrelated mount/absence | Errors give unknown/refusal; complete disjoint proof preserves safe ordinary/unrelated-root deletion |
| C23: Cancellation, depth/work/descriptor limits | Bound resources, close handles; incomplete proof refuses both effects before first removal |

C1-C14 currently have parent-reported Linux production evidence (40 scenarios,
including C10). C15-C23 remain correction/proof obligations. Native mount tests
cannot run on the reported host without CAP_SYS_ADMIN; model seams and static
source proof are not native mount execution. Windows/macOS/Go 1.24 gaps remain.

## Implementation boundary

`pkg/hfdownloader.ManagedRootSet` remains the single storage-policy owner;
`internal/server/storage_roots.go` assembles configuration/display inputs and
adapts results. Correct the negative-proof boundary under `checkProtectedEffect`
and reconcile `NestedProtectedRoots` with shared reachable-object evidence.
Preserve the existing complete `LegacyHFDeleteAllowed` call before any removal,
ordinary owned-walk/alias behavior and physical-copy/projection role separation.
The server still derives paths from #95, maps refusal and executes its existing
two-effect operation. Do not create a mount-name guard in the handler.

The implementation-local correction observes each effect's recursive directory
entry graph in bounded batches, including mounted directory entries, and compares
opened directory identities with existing protected roots or verified missing
reservation anchors. It does not globally deduplicate object identities, so
distinct namespace views are observed; active cycles, incomplete reads, identity
errors and exceeded depth/work limits refuse. `NestedProtectedRoots` uses the
same reachability evidence for existing roots. Complete Hub/friendly preflight
ordering, symlink-leaf handling, C1-C14 corrections and ordinary cleanup response
remain. See [source trace and evidence](../reviews/mount-proof-reassessment.md).
No local unit/selective delete, public selector/If-Match, lease, journal,
quarantine, rollback, transaction or new product layout policy is authorized.

## Completion evidence

Portable graph regressions and the native fixture are implemented. Preserve
C1-C14 and run focused domain/server regressions, relevant existing consumers,
full tests/race, vet and build. Implementation-local Go 1.26.7 checks pass on the
current dirty worktree. One independent tester checkpoint and full stable-diff
review remain required; bind them to the executing dirty state. The Go 1.24
native-mount workflow has not run and must pass before Ready. Native Windows and
macOS gaps remain explicit. Passing model tests do not establish native behavior.

### Native Linux CI execution boundary

`.github/workflows/ci.yml` adds a Go 1.24 Ubuntu job for focused `TestNativeMount*`
cases in `pkg/hfdownloader` and `internal/server`. The job first rejects an empty
test selection, then runs the selected tests with
`HFDESK_REQUIRE_NATIVE_MOUNTS=1` inside a private mount namespace
(`sudo unshare --mount --propagation private`). Tests using this required mode
must fail—not skip or pass without exercise—if native mount capability is
unavailable, a fixture cannot be mounted, or the expected native cases are not
actually exercised. Fixtures must use only disposable scratch-owned paths,
unmount in cleanup, and never touch project/user/system data or network/model
resources. The workflow isolates HOME, XDG config, HF cache and TMPDIR beneath
`RUNNER_TEMP`; existing general, Windows, and Docker jobs remain unchanged.

This is configured infrastructure only: no workflow run has occurred, so runner
capability and native test execution remain pending. The current local host is
Go 1.26.7 with zero effective capabilities; no mount was attempted. Go 1.24
source tracing plus later native CI evidence may support the bounded proof, but
neither selector success nor model-only tests establish native adapter behavior.

## Current implementation/evidence boundary

The clean integrated source at `7dc62163` includes correction `957498e` and the
expanded Windows selectors. Complete F1/F2 preflight and symlink/reservation
preservation have independent Linux evidence. F3 correction is now present in
the dirty implementation worktree; full native/reviewer proof remains pending.
See
[`phase-1-impl.md`](../implementation/phase-1-impl.md) for evidence provenance.
