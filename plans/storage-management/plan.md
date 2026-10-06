# HFDesk storage management

## Current phase and state

Phase 1 is split into two separately reviewed root-stage changes because the
full safety work exceeded the candidate-size limit. It is **not complete**:
F7 still blocks the root stack, and no downstream phase may begin until both
root stages close it.

- PR #111 feature branch: `feature/storage-root-ownership`, published/local
  base `0c876f38afa5cce8dc1acc5d446c254683c3e3ef`; fresh main/base is
  `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`.
- R is the facts-only package: namespace/root observations and read-side
  integration. It does not establish deletion eligibility.
- Full authorized WIP is locally retained as archive A at
  `a9931836a6517259884908d451f2975b62d2dcc0` on
  `work/storage-root-full-wip`. A is not an ancestor of the feature branch and
  must never be pushed or cherry-picked wholesale.
- Safety successor S must be based on reviewed R and selectively recover only
  its bounded safety components from A; S and the remaining F7 proof are not
  implemented in this R assignment.

## R facts contract

Capture configured path identity lexically against the recorded base. Keep
current declared root/namespace memberships distinct from filesystem object
equality, owned-entry regions, copy context and friendly-view observations.
Local type stays unknown when the Local namespace does not declare it. Unknown
or incomplete reads are errors, not empty facts. List/details metadata use one
bounded common namespace observation and preserve existing source priority,
frozen-job/config behavior, #95 HF_HUB_CACHE precedence and read DTOs.

R has no WholeCopy/LegacyHF admission API, destructive validation shim,
delete-path consumer, or safety-refusal assertion. The legacy handler/cleanup
remain byte-equivalent to the #95 base behavior; their pre-existing F7 gap is
explicitly unresolved for S. No inventory, persistence, leases, transaction,
new selector, API or UI design is introduced.

## Sequence and gates

1. Complete and commit R on the existing feature branch; keep A local and
   separate. Prove base-to-R additions plus deletions are below 5,000 lines.
2. Parent reviews R and rescope #111 metadata. No push, PR state change, Ready,
   merge, or publication is authorized in this assignment.
3. Only after R is reviewed, create S from that exact R and recover its bounded
   guards/effect integration plus E. R and S must both close F7 before any
   coordination/state phase begins.

The future phase outline remains: repository coordination/managed state;
reconciled inventory/delete engine without UI; then re-enumerated API/UI/docs.
No work in those phases is part of R.
