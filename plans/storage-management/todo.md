# Todo

- [ ] Phase 1: F3-F6 correction candidate `8ee926f629a94f21b6e1127c7a9daaba846d8ec3`
  (base/merge base `b4644b7ab33d58bf44de159b119cf5ae5ff8ea9e`); independent/native
  evidence remains blocking. Earlier full review passed with notes and the fresh
  tester covered 27 production HTTP scenarios, but the portable model-fixture
  correspondence correction and its verification are this stage's work, not yet
  independent evidence.
  - [x] Correct ordinary empty-directory EOF behavior; require complete graph
    reachability plus specific protected-intersection refusal in positive cases;
    preserve allow for a completed disjoint graph. On the worktree based on
    `8ee926f`, Linux Go 1.26.7 passed
    `go test ./pkg/hfdownloader -run 'TestManagedRootEffectProof' -count=1`,
    `go test ./pkg/hfdownloader -count=1`, and `go test ./internal/server -count=1`
    in isolated HOME/XDG/APPDATA/LOCALAPPDATA/HF/TMP. This is implementation-local
    coverage of the corrected tests and preserved package/server consumers, not
    independent evidence; no production source or native fixture changed.
  - [ ] Independent test/review of the resulting exact candidate and required
    Go 1.24 native-mount CI remain pending.
  - [x] Trace F1 inverse external-child-alias ancestry and F2 secondary friendly
    cleanup bypass; record the shared model in `phases/phase-1.md` and
    `reviews/protected-roots-reassessment.md`. This is planning, not a fix.
  - [x] Parent reconciled the source correction and integrated freshly fetched
    main `b4644b7` by ordinary merge; unrelated upstream Docker/docs/CI changes
    are retained.
  - [x] Bounded debugger correction: shared physical-ancestry observation and
    complete legacy hub/friendly eligibility before the first removal.
  - [x] Establish failing F1/F2 and missing-child-under-alias regressions;
    run available preserved package coverage for safe deletion, browsing,
    metadata, aliases and frozen destinations.
  - [x] Bounded DevOps follow-up: Windows Go 1.24 selectors span executable
    C1-C14 domain/server cases, including captured-base, friendly-priority,
    frozen destinations, F1/F2 and valid-name regressions. GitHub
    execution/skips are pending; no native pass is claimed.
  - [x] Independent tester covered C1-C14 with 40 production scenarios on Linux,
    including safe C10 permission failure (parent-reported). This excludes F3.
  - [x] Full review of `7dc62163`: changes required, F3 bind-mount namespace
    counterexample. Native mounting was unavailable, not passed.
  - [x] Record the replacement shared proof level and C15-C23 obligations in
    `phases/phase-1.md` and `reviews/mount-proof-reassessment.md` (planning only).
  - [x] Add the Go 1.24 Ubuntu native-mount CI boundary: nonempty focused test
    selection, private mount namespace, isolated scratch environment, and a
    required-native test mode. Workflow execution and fixture implementation
    remain pending; this is not native evidence.
  - [x] Bounded F3 correction: shared complete directory-effect reachability,
    protected object/missing-anchor comparison, and `NestedProtectedRoots`
    reconciliation under ManagedRootSet. F1/F2 preflight remains intact.
  - [x] Add coherent namespace-fact cases for unrelated namespace parents,
    missing anchors, repeated-object distinct views, re-exposed authorizing root,
    cycle, partial reads and unreadable entry identity. Add required-mode Linux
    Go 1.24 bind fixtures for Hub/friendly model/dataset effects.
  - [x] Correct effect enumeration to require explicit EOF (short successful
    reads continue; empty non-EOF reads fail closed), preserve missing-anchor
    identity separately from an existing protected root, and require the native
    worker's kernel mount namespace to differ from its actual parent.
  - [x] Require CI's exact positive bind fixture `TestNativeMountProtectedRootReachability`.
  - [x] Run focused/full Linux Go 1.26.7 checks locally; ordinary native fixture
    skips because this host cannot create a private mount namespace. Required
    mode fails on capability denial; neither result is a native mount or Go 1.24
    pass.
  - [ ] Independent integrated checkpoint and full stable-diff review of the
    corrected candidate, with exact base/head and platform evidence limits.
  - [ ] Parent reconcile remote CI/native-platform evidence and publication gate.
- [ ] Phase 2: repository coordination and managed HF state.
- [ ] Phase 3: reconciled inventory and deletion engine.
- [ ] Phase 4: copy/artifact selectors, conditional API, UI, and docs.

Known blockers: the corrected portable-model tests/docs have implementation-local
focused package and server coverage, but independent checkpointing is pending.
The required native-mount Go 1.24 CI job has not run. This host cannot create a private mount namespace.
Windows Go 1.24/remote CI remain pending, macOS behavior is unverified.
The F1/F2 corrections at `957498e` and current C1-C14 passes must be preserved,
but cannot be promoted to a complete filesystem-namespace proof. No layout PR,
new deletion engine, mount-support policy change or publication is authorized.
