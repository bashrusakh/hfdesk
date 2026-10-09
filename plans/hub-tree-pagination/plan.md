# Hub tree pagination: PR #121 follow-up

## Goal, authority and evidence snapshots

Preserve complete Hub tree traversal for analyze and plan/download while applying
the two user-approved corrections and the necessary integration repair below. The adopted outcomes of
[issue #96](https://github.com/bashrusakh/hfdesk/issues/96), project guidance and
the user's approval govern acceptance. Review diagnoses, proposed mechanics and
performance examples are evidence, not authority for broader policy.

- Existing owned [PR #121](https://github.com/bashrusakh/hfdesk/pull/121), author
  `bashrusakh`, branch `disco-salamander`: continue this PR, not a new PR.
  Parent-supplied publication snapshot on 2026-10-09: **Draft**, remote head
  `9d3c9ff62aca71700b590b2fa1a2094c269d5fdf`. This is not a live host-state claim;
  confirm publication state from the host/current workflow handoff before acting.
- Historical starting HEAD: `9d3c9ff62aca71700b590b2fa1a2094c269d5fdf`.
- Historical starting and reviewed base (`origin/main`):
  `97bdec0ba81b7ca30c3aba63315054eb273291ba`.
- Before initial artifact creation on 2026-10-09, read-only provenance checks verified
  a clean worktree, HEAD = `origin/disco-salamander` = PR head, and base = merge
  base = PR base. The parent supplied the freshly fetched base; no leaf fetch,
  checkout, branch update or publication was performed.
- Discovery and semantic/artifact gate: **complete**; gate session
  `ses_ee281f180ffebS8T1Fy4bPKhGq`. S1/P1 implementation and integrated independent
  workspace verification are **complete locally**. They were subsequently included
  in candidate `a5b0664afaff24e709c64c4ddcb61716d80ca643`.
- Full-PR review snapshot: `ses_ee190fbefffezJiHzPb3BA7kTL` reviewed **11/11 files**
  at clean `a5b0664` against the base above; verdict **changes required**. Structural
  pagination diagnostics containing classification-like text could still turn a
  required model-tree failure into successful dataset analysis.
- Necessary narrow repair: direct user fail-closed approval reconfirmed by gate
  `ses_ee1895d25ffetI0y9sGRHUK69h` (**proceed**). Implementation and fresh independent
  integration verification are **complete locally**. The corrected combined
  Candidate HEAD/commit, whole-PR review and candidate CI remain **pending**.
- Repair evidence snapshot on 2026-10-09: executing HEAD
  `a5b0664afaff24e709c64c4ddcb61716d80ca643` plus four modified source/test files;
  HEAD-to-worktree diff SHA-256
  `baa3c4e63d709c2d802a2dcaa55f31925dcd8658fb287fe645771a3d133b3027`.
  This identifies tested workspace contents, not a committed/published corrected
  candidate or an assertion that HEAD/dirty state will remain unchanged.
- This digest assignment permits only this file and `todo.md`. No source,
  other documentation, historical plan, commit or PR-state changes are permitted.

## Deduplicated findings and accepted outcomes

### S1: reject cross-origin advertised next before any request

[CodeRabbit security comment](https://github.com/bashrusakh/hfdesk/pull/121#discussion_r4224401872)
duplicates the independent reviewer's confirmed security finding; it is one
correction, not a second security work item. Historical starting code resolved
arbitrary absolute/network-path targets and attached Bearer authorization to fresh requests.
The parent reported real PlanRepo/Analyze fake-token probes demonstrating leakage.
S1 is complete locally and independently verified; no hosted review
thread is claimed resolved.

Reject an advertised next whose resolved origin differs from the immutable
original tree-request origin, even without a token. Valid relative and absolute
same-origin pagination must continue working. Stripping authorization while
following a foreign target does not satisfy the approved outcome.

### P1: avoid repeated complete-list coverage scans without semantic changes

[CodeRabbit performance comment](https://github.com/bashrusakh/hfdesk/pull/121#discussion_r4224401862)
identified repeated `subtreeListed(nodes, child)` scans. Starting-code inspection confirmed
worst-case O(D*N) prefix comparisons, not that every listing incurs that cost.
The 60-second handlers exist; the example large-repository timeout is unmeasured.

Reduce repeated full-list coverage scanning while preserving exactly which
directories are explicitly listed, request/callback ordering and visited behavior.
A raw-prefix ancestor map is the locally implemented mechanism, not the acceptance
criterion or authority for stronger path policy. P1 is complete locally;
no hosted review thread is claimed resolved.

### R1: propagate known structural pagination failures through public Analyze

The full-PR review at `a5b0664` reproduced invalid, malformed and cyclic next-link
failures being classified by diagnostic `401`/`unauthorized`/`not found` text as a
missing model namespace, then returning the opposite dataset's files with nil
error. The existing adopted fail-closed outcome already requires these selected
tree failures to remain failures; R1 is a necessary integration repair, not a new
general fallback policy. The semantic gate reconfirmed that authority.

The locally verified mechanism marks only malformed/empty advertised next,
resolution failure, pagination cycle and origin rejection with
`errors.Is(err, hubtree.ErrPagination)`. The existing analyzer classifier vetoes
that marker at both uses. Preserve diagnostic text and underlying parse errors;
do not mark ordinary network/status/decode/cancellation/budget/initial-URL or
callback errors. Genuine auth/not-found fallback and the optional opposite-
namespace probe after a successful model listing retain existing behavior.

## Ownership, invariants and state transitions

The smallest existing owner is `internal/hubtree`, already used by
`pkg/hfdownloader/client.go` and `pkg/smartdl/analyzer.go`. Keep shared traversal
rules there; caller file validation, dedup, metadata conversion and error mapping
retain their existing owners. R1 narrowly extends the repair boundary to the
existing `pkg/smartdl/analyzer.go` namespace classifier: `internal/hubtree` owns
recognition of structural failures, and smartdl owns how they affect autodetection.
PlanRepo already propagates these errors directly; no broader redesign is needed.

- **Origin:** per-Walk private state captures the first built tree-request origin.
  Compare parsed scheme, case-insensitive hostname and effective port (HTTP 80,
  HTTPS 443 when absent). Resolve next against the current request URL, validate
  before recording/fetching it, and never reset origin for pages, retries,
  directory fallback or recursive-query rejection. Use bracket-aware URL APIs
  for IPv6; do not introduce DNS-based equivalence or endpoint restrictions.
- **Coverage:** after all pages of one listing succeed, lazily compute a
  listing-local index once at the first validated directory. It satisfies
  `covered[child] == any(strings.HasPrefix(n.Path, child+"/"))`
  for every validated child. Index raw paths of all node types, including
  directories; never clean, case-fold or convert backslashes. A node equal to
  the child alone does not cover it; a trailing slash counts as before.
- **Traversal:** keep `cleanChildDir`, ordered depth-first traversal, callback
  failure propagation and per-Walk `visited` semantics. Coverage must not leak
  between listings or Walk calls. Do not add transactional callback rollback.
- **Failure:** foreign advertised next fails loudly without any destination
  request or retry. Known structural failures in a required listing must not
  become successful partial PlanRepo/Analyze results, opposite-namespace success,
  or a combined missing-repository error. The constant cross-origin diagnostic
  alone was insufficient for other structural failures; the R1 marker now keeps
  those failures out of both existing namespace-classifier uses.
- **Classification boundary:** preserve real auth/not-found fallback. A required
  dataset failure after genuine model fallback remains structural; an optional
  dataset-probe failure after a successful model listing remains ignored as before.
  Explicit dataset failures propagate directly. Marking is not a general taxonomy
  for transport, status, decode, cancellation, budget or initial-URL failures.
- **Preservation:** retain parser compatibility, cycle/request bounds,
  400/422 recursive fallback, retry/cancellation, unsafe FILE rejection, exact
  and case-insensitive planner dedup, commit capture and LFS size/hash semantics.

## Serial implementation batches

1. **Security, complete locally — @debugger:** immutable origin/pre-fetch guard
   and origin/walker/real-caller regressions implemented and locally checked.
2. **Performance, complete locally — @debugger serial continuation:** repeated
   scans replaced by lazy listing-local raw indexing; differential, ordered
   traversal/request tests and scaling benchmarks added. Parent reconciled S1
   preservation and both batch scopes. Executor: `ses_ee27ecafbffeueVOOFBVsO9hoO`.
3. **Integrated independent verification, complete locally — @tester:** both
   corrections, real callers and preserved behavior verified at the matching
   dirty-workspace identity. Session: `ses_ee26f7b56ffe6J9vHy07f5vp07`.
4. **Historical candidate/review — parent owner and @reviewer:** candidate
   `a5b0664` included S1/P1; full-PR review `ses_ee190fbefffezJiHzPb3BA7kTL`
   returned **changes required**, exposing the public Analyze integration gap.
5. **Necessary integration repair, complete locally — @debugger:**
   `ses_ee187c5fdffe7K5G1c0q0PogrM` added narrowly scoped structural marking and
   the classifier veto, plus public failure/fallback regressions. No generalized
   network/status classification or optional-probe change.
6. **Fresh independent integration verification, complete locally — @tester:**
   `ses_ee1814bdcfferGfdG19awetUw6` matched repair contents, checked the required
   public boundary and preservation cases, and reran S1/P1 plus full validation.
7. **Corrected combined candidate commit, whole-PR review, candidate CI/publication, pending — parent owner:**
   refresh state identity and bind applicable validation/review to exact base +
   candidate HEAD. Prior approval cannot certify changed code. This plan grants
   no commit, push, Ready or other publication authority.

Historical S1/P1 implementation files: `internal/hubtree/hubtree.go`,
`internal/hubtree/hubtree_test.go`, `pkg/hfdownloader/treewalk_test.go`,
`pkg/smartdl/treewalk_test.go`, plus `internal/hubtree/hubtree_bench_test.go`.
R1 repair snapshot files: `internal/hubtree/hubtree.go`,
`internal/hubtree/hubtree_test.go`, `pkg/smartdl/analyzer.go`,
`pkg/smartdl/treewalk_test.go`. The smartdl classifier is now intentionally touched;
downloader adapters and server code remain outside the repair.
No API shape, UI, dependency, persistence or timeout change is necessary.

## Unified verification matrix

| Case / transition | Required evidence |
| --- | --- |
| Foreign absolute/network-path next; changed host/port; HTTPS to HTTP | Error and **zero destination requests**, not just missing Authorization |
| Real PlanRepo and public Analyze; models/datasets; fake token/no token | Unsafe selected-tree pagination rejected without successful partial result |
| Query-only, root/path-relative, absolute and network-path same-origin next | Complete file set, expected requests and authorization when configured; preserve URL/query resolution |
| Hostname case, default/explicit equivalent ports, IPv6 literals | Deterministic parsed-origin cases; bracket-safe hostname/port handling |
| Unsafe next on later page or directory listing; recursive fallback | Immutable original origin and zero unsafe request/retry |
| Malformed/empty/resolution/cycle/origin failure; diagnostic `401`, `unauthorized`, `not found`; first/later/directory listing | Structural marker survives wrapping/parse errors; required public Analyze fails with nil result, no opposite success or subsequent metadata requests |
| Genuine model 401/404 then required dataset structural failure; explicit dataset failure | Preserve structural error, not combined missing-repository classification |
| Genuine auth/not-found fallback; ordinary 403; both namespaces exist | Existing fallback, failure and ErrBothExist behavior preserved |
| Successful model then optional dataset structural failure | Preserve successful model analysis, metadata, revision, commit and LFS semantics |
| Network/status/decode/cancel/budget/initial-URL/callback failure | No new structural marker or generalized classification policy |
| Descendant on later page; directory-only descendant; child itself | Complete-list coverage with identical explicit-directory decisions |
| Empty/duplicate/deep dirs; raw/cleaned mismatch; `a` vs `a2`; repeated/trailing slashes; dot segments; backslashes | Differential equality with old raw-prefix predicate, unchanged visited/order/request behavior |
| Mixed recursive listing and one-level mirror; callback failure | Exact request sequence and callback order; identical stopping behavior |
| Existing issue #96 regression suites | Parser/cycles, 400/422 fallback, retry/backoff/cancellation/budget, unsafe files, dedup, commit and LFS behavior preserved |
| Wide directory-first and deep/shared-ancestor fixtures | Deterministic equivalence plus production-helper benchmarks including index construction, lookups and allocations; no latency pass threshold |
| Concurrent/repeated Walks | No cross-call origin/coverage state leakage; race evidence |

### HISTORICAL S1/P1 verification snapshot

Evidence below records the original executor/tester sessions and parent
reconciliation before candidate `a5b0664`. The earlier digest matched those
content hashes. These are historical precommit snapshots, not live HEAD/worktree
or publication claims; this plan-only update did not rerun product tests.

- Historical executing HEAD: `9d3c9ff62aca71700b590b2fa1a2094c269d5fdf` plus the dirty files
  listed above; parent-supplied base: `97bdec0ba81b7ca30c3aba63315054eb273291ba`.
- Combined tracked implementation diff SHA-256:
  `4eda434fe2f522a204258f14264f53c3fb12280b7ff3c5d71ee40af3ebbfc61d`.
- Untracked benchmark SHA-256:
  `c1ba3a6510fb5a232b0caf3576f5800cd05580b501c1f0fb6160058915273a13`.
  Tester matched both before/after verification; digest matched both before its
  plan-only edits. Plan text is outside these implementation hashes.
- Environment: Linux/amd64, **Go 1.26.7**. Not Go 1.25 verification, remote CI
  evidence, or proof of a future committed/published candidate.
- Executor and independent tester passed focused packages, `go test ./... -count=1`,
  `go test ./... -race -count=1`, `go vet ./...`, binary build and formatting.
  Focused command: `go test ./internal/hubtree ./pkg/hfdownloader ./pkg/smartdl -count=1`.
  Executor built to `/dev/null`; tester built into its subsequently cleaned scratch
  workspace. Non-writing formatting checks passed; executor also passed diff checks.
- S1: **112 independent public PlanRepo/Analyze cases** covering models/datasets,
  fake/no token, foreign origins, later pages/directories/retries/fallback passed:
  stable error, nil result, **zero foreign requests**, no erroneous opposite probe.
  Positive query/root/path-relative, absolute and network-path same-origin cases
  returned complete ordered files with preserved commit/auth/query handling.
- P1: **1,609,400 production-helper differential comparisons** passed. The independent
  old-predicate oracle also passed **804 real-HTTP request/callback traces**, including
  raw/cleaned mismatches, all node types, duplicates, slash/backslash/case cases,
  pagination and callback failure. Existing preserved suites in the matrix passed.
- Isolated browser smoke showed two paginated files totaling **16 B**. Unsafe
  analysis returned **HTTP 500**, stable pagination error and zero foreign requests.
  This does not prove actual downloads or full browser negative-error presentation.
- Tester benchmarks (construction + lookups, three runs): wide-5000 median about
  **405 ms -> 2.33 ms**, about **437 KB / 48 allocations**; depth-32 about
  **11.8 us -> 23.5 us**, slower. No uniform speedup, path-independent O(N) or SLA claim.
  Command: `go test ./internal/hubtree -run '^$' -bench '^BenchmarkSubtreeCoverage$' -benchmem -benchtime=100ms -count=3`.
- Not run: live Hub, actual downloads, Go 1.25, remote CI or full browser negative
  presentation. No product failures were observed in that executed boundary;
  the subsequent whole-PR review found the R1 gap. The tester
  corrected an external smoke-helper proxy-type setup error; fake search was
  intentionally absent and caused an unrelated sidebar search error.

Discovery's in-memory raw-prefix model passed 55,575 equivalence checks; it was
not production execution. Directory-first fixtures with D directories followed
by one file per directory require D*D + D*(D+1)/2 old comparisons (1,500,500 at
D=1,000). These support the hypothesis, not latency or implemented-candidate
claims. This paragraph records historical discovery, not the completed production
verification summarized above.

### R1 local verification SNAPSHOT (2026-10-09)

This digest read the review, semantic gate, debugger and fresh tester final
reports and independently matched the repair diff hash before its plan-only edits.
Evidence applies to `a5b0664afaff24e709c64c4ddcb61716d80ca643` plus the four R1
files listed above, against parent-supplied base `97bdec0ba81b7ca30c3aba63315054eb273291ba`.
Repair diff SHA-256: `baa3c4e63d709c2d802a2dcaa55f31925dcd8658fb287fe645771a3d133b3027`.
Plan text is excluded from that source-diff identity; no artifact self-hash is claimed.

- Debugger reproduced false dataset success/combined-not-found failures before
  the repair; afterward **270 public-analysis cases**, **16 preservation cases**
  and **14 marker-scope cases** passed.
- Fresh independent tester matched contents before/after verification and passed
  the **270 public cases** plus **180 external production-caller cases**:
  Analyze/AnalyzeWithRevision, successful opposite fixtures, fake/no token,
  exact request counts, nil required-analysis result, zero foreign requests,
  wrapped marker/underlying parse errors and no metadata requests after required
  failure. Dataset failure after genuine model fallback stayed structural.
- **16 fallback-preservation cases** passed, including genuine 401/404, ordinary
  403, optional probing and ErrBothExist. Optional-probe failures retained model
  metadata, LFS size/hash, revision and commit. Marker scope covered all known
  structural cases and excluded nonstructural status/decode/network/cancel/
  budget/initial-URL/callback errors.
- Existing S1/P1 suites passed on the repaired workspace: zero-foreign-request
  origin/transition cases, **1,609,400** production index-versus-old-scan
  comparisons, request/callback ordering, listing-local coverage, complete
  pagination, backslash handling, LFS/commit and cancellation; both consumers ran.
- Debugger and fresh tester passed focused checks, `go test ./... -count=1`,
  `go test ./... -race -count=1`, `go vet ./...`,
  `go build -o /dev/null ./cmd/hfdesk`, non-writing formatting and diff checks.
  Environment: **Linux, Go 1.26.7**; no Go 1.25 or corrected-candidate remote CI
  evidence. Fresh tester did not run browser smoke, live Hub or Windows execution.
- Independent external probes passed after removing an unused scratch import;
  that first-attempt compile error was not a product failure. Owned scratch and
  fixture servers were cleaned. No new in-scope product failure was reported.
- The last full-PR verdict remains **changes required** on the earlier clean
  `a5b0664` candidate. This local pass does not replace corrected-candidate
  whole-PR review or CI, certify publication, or resolve hosted review threads.

## Risks, non-goals and semantic checkpoint

Cross-origin-pagination mirrors intentionally become fail-loud failures;
configured HTTP mirrors and valid same-origin pagination remain supported.
Indexing adds listing-local memory; assess allocations and deep-path behavior.
Avoid an unconditional O(N) claim independent of path length/depth and hashing.

Out of scope: HTTP redirect credential policy, DNS rebinding, configured-endpoint
restrictions, generalized analyzer error classification beyond the R1 marker veto, timeout changes,
latency SLA, unrelated cache/UI/storage work and the historical
`plans/download-type-folders/` plan. Public Analyze's existing opposite-namespace
probe can ignore errors after a successful model tree; do not silently strengthen
that optional-probe behavior.

Report-only adjacent finding: an ordinary 403 diagnostic whose repository URL
contains `401` can still trigger the preexisting substring classifier. It is not
a structural pagination failure and remains outside the approved repair; do not
silently generalize R1 to fix it.

A verified candidate may claim that advertised pagination cannot initiate a
fresh cross-origin request, and that the coverage optimization preserves explicit
directory-listing decisions. It may not claim general SSRF prevention, redirect
safety or guaranteed completion within 60 seconds. Escalate any material new
policy/scope choice to the parent before expanding implementation.

## Continuation and resources

Read this file and `todo.md` before resuming. Next safe action: parent establishes
an authorized corrected combined candidate commit from S1/P1 plus R1, then dispatches
whole-PR review against the refreshed base and exact candidate. Reconcile evidence
freshness and candidate CI/publication separately; do not predict approval or CI.
No unresolved product decision blocks the approved boundary.

Historical S1/P1 tester cleaned its owned processes, scratch files, certificates,
binaries and isolated app state; its browser result tab was intentionally retained.
R1 debugger and fresh tester cleaned their owned scratch/fixture resources.
Executor test servers were closed with no disposable outputs retained. The digest role
created no temporary resources and retains only the two authorized durable plan
files; it performed no source, commit, PR-metadata or hosted-thread changes.
