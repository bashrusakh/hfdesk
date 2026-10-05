# Mount-namespace / actual-effect proof reassessment

Status: **changes required; planning only**. Inspected clean candidate
`7dc62163e9ac5dffc5b126fcd18ce8ec7c212fee` on
`feature/storage-root-ownership`; base/merge base/main
`b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`.
No source/config/test/branch edits, mounts, tooling installation, agents,
commits or publication occur in this stage. F1/F2 correction `957498e` and the
complete Hub/friendly preflight are to be preserved, not restarted.

## Authority, current evidence and counterexample

The unchanged phase-1 outcome is protection of every configured root/reservation
across every intended legacy whole-HF removal effect before any mutation.
Definitions/IDs stay lexical; #95 ENV precedence, frozen jobs, alias browsing,
Issue #76 closure and the existing public endpoint/unit are unchanged.

Parent reports independent verification of 40 production scenarios on `7dc62163`
passed Linux C1-C14, including real C10 secondary permission failure reported
as 200 `success:false`. Full 17-file review still requires changes for F3.
Neither that run nor this reassessment performed native mount reproduction;
the review host lacks CAP_SYS_ADMIN. Go 1.26.7/Linux evidence is not Go 1.24 or
native Windows/macOS evidence. No new current tester/session identifier or
retained evidence path is supplied here; do not invent one.

**F3 high:** T=`/store/hub/models--owner--model`, C=`T/application-data`,
protected S=`/protected-view` is a Linux bind mount of C. EvalSymlinks preserves
the T/S spellings. Stat(S) identifies C, but the ancestor walk of S visits the
mount namespace's parents, not T; the reverse walk misses S too.
`root_domain.go:286-326` returns false/nil, `checkProtectedEffect:757-774`
permits, and `NestedProtectedRoots:408-442` misses S. Walking T's actual child
C can nevertheless match S with SameFile: inventory and eligibility disagree.
The same counterexample applies to the friendly effect. For S/next absent,
the existing mounted anchor is still C and cannot be discarded from protection.

The handler now correctly calls `LegacyHFDeleteAllowed` before its first removal
(`api.go:1706,1729`); friendly recursion remains `1766`. Do not undo that ordering.
The remaining error is the negative proof inside the shared owner.

Go 1.24.0 source evidence:

- [removeall_at.go](https://github.com/golang/go/blob/go1.24.0/src/os/removeall_at.go):
  Unix opens directory entries with O_DIRECTORY/O_NOFOLLOW, enumerates and
  recursively removes contents. Bind mounts are directories, not symlinks;
  there is no device/mount-boundary exclusion. Final rmdir can fail after bytes
  were removed. EBUSY is not protection.
- [removeall_noat.go](https://github.com/golang/go/blob/go1.24.0/src/os/removeall_noat.go)
  and [types_windows.go](https://github.com/golang/go/blob/go1.24.0/src/os/types_windows.go):
  non-Unix recursion depends on Remove/Lstat/IsDir semantics. Windows surrogate
  reparse points have distinct mode handling; a junction is not automatically
  a traversed Linux bind. Preserve leaf-removal behavior and test native facts.
- Windows SameFile can return false if deferred identity loading fails. Negative
  evidence requires established identity (for example opened-object Stat), not
  a bool produced by unavailable identity facts.

## Solution levels compared

| Level | What it proves / limitation | Choice |
|---|---|---|
| More canonicalization, reverse ancestors or device comparisons | Positive naming/equality evidence; F3 still defeats negative search | Reject as sufficient permission |
| Platform mount-namespace tables/capability authority | Could prove additional edges, but requires OS-specific completeness, privileges/view validation and Windows equivalents; fallback unknown would be necessary | Not the primary repair; escalate before a new native-authority subsystem |
| Reject all mounts or aliases | Overbroad product/FS-support change; loses safe ordinary cases | Reject |
| Observe directories actually reachable by each recursive removal and match protected objects/anchors | Covers bind aliases using existing metadata/relative operations; incomplete traversal becomes unknown | Chosen closest existing boundary, subject to proof/verification |

This reconsiders the earlier subtree-observation alternative because the new
system counterexample shows name ancestry cannot guarantee the outcome.
Durability, leases and quarantine do not supply missing namespace reachability.

## Bounded proof model under ManagedRootSet

1. Keep configured-definition containment and destructive path/role guards.
   Gather protected-root observations: existing directory object, or deepest
   verified existing directory anchor plus ordinary missing suffix. Keep errors,
   dangling/reparse ambiguity and absent-effect evidence distinct.
2. For each existing primary/secondary effect, observe its actual recursive
   directory-entry closure under the installed remover's semantics. Include
   directory mount edges; do not skip devices or use the owned walker, which
   excludes protected roots. Do not read model bytes or enumerate unrelated
   filesystem trees. Symlink leaves are entry effects, not target-content edges.
3. Compare reached directory objects with protected objects/anchors using
   established physical identity. An external bind alias of C is caught because
   C is reached, irrespective of S's parent spelling. A bind inside T exposing
   an externally configured root is caught at that reached directory. Preserve
   missing reservations beneath reached anchors; no fictional child identity.
4. Ordinary authorizing namespaces may be ancestors of the selected repo, but
   their protected objects cannot be exempted if a mount re-exposes the namespace
   itself inside the effect. Existing root-ID exemptions must not hide that hit.
5. Known outside requires completed applicable edge/object coverage plus valid
   reservation evidence. The absence of a match in a truncated/erroring scan is
   unknown, not outside. This proves operation-specific nonintersection, not
   universal physical nonancestry of paths in every namespace.
6. Distinct occurrences of one directory object can have different child mount
   views. Do not globally deduplicate by SameFile and skip later namespaces.
   Detect active cycles or otherwise unestablished termination; refuse rather
   than silently prune them and declare complete. Stream enumeration and bound
   live handles/memory/work via existing request/cancellation facilities; any
   exceeded bound or observation failure prevents both removals. No new settings
   or hidden partial-proof allowance.

The observation is ephemeral, not a durable graph or list of authorized delete
actions. Use Go 1.24 os.Root relative reads/opened-object facts where they cover
the observation; Root permits mounts and is not the proof by itself. Do not
require unavailable Root.Rename/RemoveAll, version bumps, dependencies or tools.
Keep the production RemoveAll execution and complete-effect admission boundary.
The proof assumes the observed namespace; no atomicity/noncooperating-writer
guarantee is added. Relevant observed changes must invalidate evidence.

Reuse the same reachable-object evidence for `NestedProtectedRoots` so it cannot
claim no nested protection while owned walking recognizes the object. Preserve
symlink-leaf browsing ownership and ordinary alias handling. Do not force every
read-only missing-path lookup into a destructive proof or add per-handler rules.

## State and preservation obligations

See phase 1 C15-C23 alongside the previously passing C1-C14:

| Observation state | Outcome |
|---|---|
| Reached protected directory or implicated missing reservation | Refuse before either removal; retain both trees and sentinels |
| Mount/alias encountered, proof remains complete and disjoint | Do not blanket-refuse merely because it is mounted |
| Unknown identity/entry semantics, incomplete enumeration, cycle or exhausted bound | Refuse with no mutation; close observation resources |
| Both effects complete/disjoint; friendly absent verified | Preserve normal model/dataset/external-H behavior |
| Safe allowed operation later has secondary I/O failure | Preserve visible C10 partial-failure behavior; no rollback claim |

Plain friendly symlinks must not become traversal edges. Reading proof metadata
adds cost and may expose genuine unknowns: do not hide unreadable directories
to keep a success test green. Conversely preserve C10 where enumeration facts
are available but cleanup lacks write permission. Resource limits are proof
limits, not new claims that large/mounted repositories are unsupported.

## Debugger assignment (parent dispatch only)

Start from parent-confirmed `b4644b7..7dc62163` and the current task branch.
Allowed correction boundary: `pkg/hfdownloader/root_domain.go`, its focused
tests and a small private namespace-observation seam; existing server adapter/
preflight callers and tests only as needed to carry that shared proof. Keep
complete preflight and all C1-C14 fixes. No layout, API/UI, new delete unit,
If-Match, lease, journal, quarantine or phase-3 execution framework.

- First trace actual Go 1.24 remover edges against the proposed observation;
  state what completed negative proof establishes. Establish F3 red cases,
  missing mounted-anchor reservation and an inward mount to a protected root.
- A deterministic seam supplies coherent directory entries, namespace-local
  parents, identity/absence/errors, not precomputed contains/allowed answers.
  Reuse real temporary-object FileInfo/identity where possible; one backing
  directory may appear through multiple views with different children. Ensure
  old ancestor logic fails those fixtures before declaring regression coverage.
- Implement/consolidate the proof under the existing owner; classify errors and
  partial scans unknown; reconcile NestedProtectedRoots and both legacy effects.
  Report costs, handle cleanup and preservation results without new semantics.
- Run focused/full/race/vet/build and existing consumers, preserving the 40-case
  production boundary. Return exact base/head/dirty state, changed-case map and
  honest native/toolchain gaps. Stop for a material new native capability or
  domain/support-policy requirement; do not manufacture an architecture pass.

## Verification without mount privilege

A proposed faithful graph seam must establish deterministic C15-C23 coverage,
including different namespace parents, missing suffixes, inward mounts, repeated
object views, cycles and partial reads. Production adapters also need real ordinary,
symlink, permission and object-identity fixtures. These are model/adapter
evidence, not native bind execution. No mount attempt/install occurs here.

Add capability-gated native Linux fixtures on an existing suitable CI/test host
using available standard-library OS support, disposable mountpoints and explicit
unmount/cleanup ownership. Capability denial is a reported skip/blocker, never
passed evidence. Native Windows tests must exercise the actual junction/reparse
classification and identity-loading behavior; do not emulate Linux traversal.
Any CI selector follow-up is a separate bounded DevOps assignment after names
stabilize, preserving current Go 1.24, Windows, Docker and state-writer checks.

One integrated independent tester checkpoint then full stable-diff review is
required after correction. Native mount and platform gaps stay explicit even if
portable model/adapter proof is sufficient for a narrower claim. Phase 2 and
publication remain blocked on the foundation evidence; no gate is passed here.
