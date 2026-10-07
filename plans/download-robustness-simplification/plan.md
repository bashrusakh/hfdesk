# PR117 downloader robustness simplification

## Accepted scope

Implement the settled local-only PR117 changes on `fix/download-robustness`:

1. Make transfer status handling retry only `429` and all `5xx`; preserve the
   distinct job-fatal behavior of `401`/`403`/`404`, multipart `206` protocol,
   resume data, and optional verification metadata semantics.
2. Bound progress-refunded attempts with overflow-safe arithmetic and never
   refund ignored-Range restarts.
3. Retain context-aware hashing and atomic staged cache publication, including
   the existing fallback on any rename error; test failure/cancellation after
   staged bytes have actually been copied.
4. Keep `stallTimeout` as the sole new server setting. Restore server retry
   backoff defaults to 400ms/10s and do not expose those library controls via
   API settings.
5. Keep API/changelog descriptions aligned and simplify tests/comments that
   incorrectly prohibit already-complete sibling publication.

At the initial implementation boundary, no commit, push, branch/history
change, PR-state change, issue publication, CodeQL dismissal, or external
review was in scope. The workflow owner later authorized one local commit of
the accepted implementation; push and PR-state actions remain outside this
assignment.

## Status

- Accepted behavior and local-only boundary: settled by the workflow owner.
- Mutation baseline: authorized existing PR branch, clean at
  `5923e8369c6db2180c88c3df4acda7ce8fc7e1bf`; base
  `origin/main` at `45d4de0c9d08fcc44dc1a24a00c7c703cface892`, seven commits
  ahead and zero behind.
- Implementation: complete within the accepted scope; atomic cache staging
  remains enabled for every rename fallback, and the context-aware store helper
  is package-private while the old public API remains intact.
- Validation: focused downloader/server tests, full tests, race tests, vet,
  build, formatting, and whitespace checks all pass on the resulting worktree.
- Documentation follow-up: retry-ceiling, ignored-Range refund, and atomic-
  fallback wording corrected in `docs/API.md` and downloader comments. No
  executable behavior changed. The tracked full-diff hash is now
  `23a6d1a968423755c097cb331594a6c0b2ab997116a14706ff4f0364c51f452f`.
- Independent review: Sol6.1 reviewed the full 23-file change plus this plan
  after the wording correction: **PASS WITH NOTES**, with no code or
  documentation blocker. Managed OCR was not run and is outside this evidence.
- Independent local/API/browser smoke passed against the runtime code at the
  preceding tracked-diff hash `44e3285aaa88502aec40a7a132632cf8b0639da72545365ec27a55f06ac17e9a`.
  The workflow owner reconciled the only subsequent runtime-file delta as
  comments/prose, with no executable change, so this smoke remains applicable
  to the current tracked diff.
  - A 65,536-byte transfer resumed from 8,192 bytes with one `Range: bytes=8192-65535`
    request, then completed with the expected bytes/hash.
  - Actual GET `403` and `410` each produced one request and an actionable
    failed job; optional verification `HEAD 403` remained benign.
  - Same-size content corruption failed SHA verification and did not publish a
    final file. Linux snapshot/ref/deduplication behavior was valid.
  - API checks covered `stallTimeout` zero, invalid input, preserve behavior,
    and protective `60s` after restart. Browser pause/resume/cancel confirmed
    partial-file and job-state behavior for API-started jobs; full search/analyze
    flow was not exercised.
  - No Windows, multipart, or forced cache-copy-fallback manual smoke was run;
    relevant automated coverage passed. An initial harness assumption about an
    open-ended Range response was corrected to the server's bounded Range and
    was not a product failure. The external verification helper made no
    project changes.
- The tester's temporary processes and directory were cleaned. Its browser
  health-JSON tab remains open because the browser tool has no close action.
- At the authorized commit boundary, the worktree is based on
  `5923e8369c6db2180c88c3df4acda7ce8fc7e1bf`, with base
  `45d4de0c9d08fcc44dc1a24a00c7c703cface892`, and contains the accepted
  tracked-diff hash above plus this plan. The workflow owner authorized one
  local commit of exactly these changes. Push remains with the workflow owner,
  after base provenance refresh and final whole-candidate review; the PR remains
  Draft.
- Full tests, race tests, vet, build, and focused regression tests passed on the
  unchanged runtime code. The later documentation/comment edits and this plan
  status update do not change executable behavior; full checks were not rerun
  for those prose-only changes.
- Separate latent remote-hash/path CodeQL risk remains for triage; this review
  and smoke do not imply blanket Ready approval. No publication or history
  action is authorized here beyond the single local commit.
