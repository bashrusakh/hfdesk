# Todo

- [ ] Complete facts-only R on `feature/storage-root-ownership` from `0c876f38`.
  - [x] Preserve all pre-split WIP on local archive A `a9931836` (not an R
    ancestor; never push/cherry-pick wholesale).
  - [x] Remove destructive authorizers, deletion consumers and safety assertions
    from the R working tree; restore the legacy delete source block to `b4644b7`.
  - [x] Reconstruct bounded namespace/root/owned-entry facts and common read-side
    integration; retain logical IDs/roles, unknown Local type and error states.
  - [x] Replace native Linux positive permission check with real bind-region
    observation; keep isolation check and case-private fixture. Preserve the
    valid relative-route fixture for Windows.
  - [x] Run focused downloader/server tests; both pass on Linux Go 1.26.7.
  - [x] Run focused packages, `go test ./...`, `go test ./... -race`,
    `go vet ./...`, `go build ./cmd/hfdesk`, Windows test cross-builds and the
    existing Windows selector locally. All passed on Linux Go 1.26.7; cross-build
    and selector runs are not native Windows evidence.
  - [x] Attempt required-native `TestNativeMount*`; worker-marker negative case
    passed, but positive subprocess launch failed `operation not permitted` before
    namespace setup. No native mount/Go 1.24 pass is claimed.
  - [x] Historical source commit `4bcf7f9` was reviewed as requiring changes;
    archive A is not its ancestor. That review is not approval of corrected R.
  - [x] Commit corrected source/test/CI bytes and status snapshot as
    `ff66636`; the completed candidate and status records remain below 5,000
    additions plus deletions.
  - [ ] Parent refreshes verification, reviews complete corrected R and rescope
    of owned Draft #111. Publication and Ready remain unauthorized.
- [ ] Build Safety S from reviewed R, selectively recover only bounded guards,
  destructive integration and safety tests from A, implement E and close F7.
- [ ] Start coordination/state work only after both root-stage foundations are
  reviewed and F7 is closed.
- [ ] Future inventory/delete engine; then API/UI/docs.

F7 remains blocking. No S/E implementation, PR update, push, Ready, merge or
publication is part of this task. A is intentionally retained locally for parent
handoff; its cleanup is not authorized before all unique components are accounted
for.
