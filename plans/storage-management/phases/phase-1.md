# Phase 1 — root/path foundation

Status: **open; F7 remains blocking**. Deliver in two root-stage changes:
facts/read-side R, then safety S. The code present in R is not deletion
eligibility, and the legacy whole-HF handler retains the #95 behavior (including
the known unresolved F7 case) until S is complete.

## R: configured namespace facts

- Root IDs use cleaned lexical paths against a captured base; physical object
  facts are fresh observations and never rewrite configured identity.
- Observe all current declared Hub, Local and projection slots before filtering
  by requested repo. Preserve each root/role/repo-ID membership even when paths
  compare equal. Local repository type remains unknown unless declared by the
  namespace.
- Record owned directory regions and entries with bounded, EOF-complete reads.
  Nested configured roots are distinct memberships and excluded from the
  enclosing root's owned entries. Unknown identity/read failures stay errors.
- Scan, details and GGUF metadata project the same observation into existing
  DTOs; preserve source priority, API shape, config/path snapshots, frozen jobs
  and HF_HUB_CACHE precedence.
- R exposes no destructive authorizer or compatibility shim and does not wire
  facts into the delete call graph. Root/effect graph queries are descriptive.

## S: safety successor, not part of R

Starting from reviewed R, S must consume complete current membership and region
facts before any legacy Hub or friendly removal. It must handle foreign repo IDs
and descendants, independent same-UID copies, mixed roles/projections, unknown
context and incomplete observations; refusal must occur before both effects.
Preserve existing supported behavior, symlink-leaf semantics, F1-F6/EOF/worker
isolation and C20 distinct namespace views. Do not add inventory, leases,
journals, quarantine, new delete units/APIs or hostile-writer guarantees.

## Verification map

| R observation | Evidence |
|---|---|
| Lexical definitions, multi-role memberships, unknown Local type | Root-domain and namespace tests |
| Foreign repo-ID aliases, same-object/different-view memberships | Namespace membership and actual bind-region observation tests |
| Nested-root ownership, partial read and EOF behavior | Owned-entry and directory-observer tests |
| Read-side list/details/source/metadata preservation | `go test ./internal/server` plus downloader consumers |
| No safety API/consumer/assertion in R | Source/call-graph search and moved-test inventory in A |

Native Linux mount execution is capability-dependent; Windows cross-build is not
native Windows evidence. Record exact skips/failures. F7 stays open until the
complete R+S root stack has independent review and applicable native evidence.
