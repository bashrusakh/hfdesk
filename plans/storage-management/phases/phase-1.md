# Phase 1 — root/path ownership

## Contract

Represent configured paths by cleaned lexical identity resolved against an explicit captured base; do not use `EvalSymlinks` or infer case-insensitivity from GOOS. Compare existing physical objects only with filesystem evidence (`os.Stat`/`os.SameFile`); missing or unreadable paths have unknown physical equality. Build immutable, operation-scoped root definitions from effective cache/HF and configured friendly/local/scan/route roots. Merge role restrictions when the same physical object is proven. Give nested configured roots exclusive ownership, including aliases only when ancestry is established from fresh filesystem observations; fail closed when it is not.

Keep read-only browsing and destructive authority separate. Preserve representative list/details behavior and friendly orphan visibility. No public physical-copy selectors or transactional deletion changes in this phase.

## Regression map

| Case | Verification |
|---|---|
| Missing/case-distinct lexical roots, normalized relative roots | domain and server tests |
| Existing same-object aliases; missing identity remains distinct | filesystem-fact tests |
| Merged role restrictions and stable root identity | root-set tests |
| Nested configured roots, physical ancestry via alias, and prefix siblings | scanner ownership, size/count/GGUF metadata, finder and details tests |
| Alias browsing and symlink root/owner/repo destructive refusal | real API listing/details plus pure eligibility and refusal tests |
| Whole Hub exact repository shape, protected nested-root refusal, model/dataset and external H support | existing delete handler and new end-to-end handler tests |
| Existing HF/friendly prioritization, raw cache restrictions, and frozen R/H job identity | focused server regression suite including Hub association dedup/cleanup |
| Manifest association is physical filesystem evidence and remains read-only historical evidence | symlink-alias SameFile regression |

## Implementation boundary

`ManagedRootSet` owns stable lexical IDs and immutable path/role definitions for one config generation. It captures an explicit base; fresh `Stat`/`SameFile` observations establish physical equality/ancestry per operation. `OwnerForPath`, `NestedProtectedRoots`, root-scoped `Resolve`, and non-mutating `WholeCopyAllowed` are the authority surface. Browser scanning, qualification, size/file accounting, GGUF/mmproj metadata, local finder/details, and the existing whole-Hub delete refusal all use this owner. No local delete unit, copy selector, transaction/quarantine, or API/UI contract was added.
