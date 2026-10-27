// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Regression coverage for the WINDOW of the domain-exclusive publication tier
// (issue #140 / PR #151).
//
// The domain-exclusive tier places a content-addressed blob with a
// check-then-rename: it stats the target and, when the target is absent,
// renames the payload onto it. That rename REPLACES an existing target, so
// the tier's whole guarantee rests on ONE precondition:
//
//	while the window between the existence check and the rename is open, no
//	DOMAIN PARTICIPANT can create the target.
//
// The precondition holds because the placement runs while the caller holds
// the blob key's ownership domain, and that domain excludes every other
// participant of the same key. The property is therefore not "the placement
// refuses to replace an existing target" - the placement's own existence
// check already covers that, and the pre-#140 code had it too - but "no
// domain participant can create the target between the check and the
// rename", which is what makes the tier's no-replace outcome for that
// participant guaranteed instead of incidental.
//
// The test below pins that exclusion with a seam between the existence check
// and the rename (blobPublishWindowHook): while the placement holds the
// domain, a second writer of the SAME blob key - a domain participant, like
// the other route that writes this key - tries to publish its own payload
// inside the window. The domain must exclude it, so the target cannot appear
// there, and the second participant's own later placement must refuse to
// replace the published blob.
//
// What is NOT guaranteed, and is deliberately NOT claimed here: a writer
// OUTSIDE the domain - the mirror writer - is not fenced by the ownership
// domain, so it can still create the target in the window. That limitation
// is documented (doc.go, AGENTS.md, CHANGELOG.md) and is not fenced by this
// change; the downloader's publication invariant does not depend on it.
//
// The negative control reconstructs the pre-#140 model through the production
// seams - the Stat-then-rename placement and no shared ownership domain for
// the blob key - and shows that the SAME property fails there: the second
// writer publishes inside the window and its proven blob is replaced.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// windowPublisherBytes and windowParticipantBytes are the two byte streams of
// the publication-window scenario: the publisher's payload and the other
// participant's payload.
var (
	windowPublisherBytes   = []byte("the publisher's bytes")
	windowParticipantBytes = []byte("the other participant's bytes")
)

// windowScenario is what one run of the publication-window scenario observed.
type windowScenario struct {
	// publishErr is the publisher's placement error.
	publishErr error
	// published holds the target's bytes at the end of the run.
	published []byte
	// targetExists reports whether the target exists at the end of the run.
	targetExists bool
	// participantBlocked reports that the second participant was excluded by
	// the ownership domain instead of publishing inside the window.
	participantBlocked bool
	// participantPublishedInWindow reports that the second participant
	// published the target inside the placement's window.
	participantPublishedInWindow bool
	// participantPublishedBytes holds the bytes the second participant found
	// at the target right after its own publication.
	participantPublishedBytes []byte
	// participantErr is the second participant's placement error.
	participantErr error
}

// runPublicationWindowScenario runs the scenario the domain-exclusive tier
// depends on.
//
// The publisher holds the blob key's ownership domain and places its payload
// through the placement. While the placement's window between its existence
// check and its rename is open, a second writer of the SAME blob key - a
// DOMAIN PARTICIPANT, exactly like the other route that writes this key -
// tries to publish its own payload there.
//
// The run records what it observed: whether the second participant was
// excluded by the domain, whether it published inside the window, and what
// the target holds afterwards.
func runPublicationWindowScenario(t *testing.T, repo *RepoDir, sha string) windowScenario {
	t.Helper()

	blobPath := repo.BlobPath(sha)
	domainDst := mustDomainDst(t, repo, sha)
	publisherSrc := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".part")
	participantSrc := filepath.Join(repo.BlobsDir(), "tmp-"+sha+".other")
	if err := os.WriteFile(publisherSrc, windowPublisherBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(participantSrc, windowParticipantBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// The publisher holds the blob key's ownership domain for the whole
	// placement, like every writer of blobs/<sha>.
	domain, err := blobDomainAcquire(t.Context(), blobPath, domainDst)
	if err != nil {
		t.Fatal(err)
	}
	defer domain.release()

	// The second participant's outcomes.
	participantWaiting := make(chan struct{}, 1)
	participantPublished := make(chan struct{})
	participantErrCh := make(chan error, 1)

	oldContention := blobDomainContentionHook
	blobDomainContentionHook = func(string) {
		select {
		case participantWaiting <- struct{}{}:
		default:
		}
	}
	defer func() { blobDomainContentionHook = oldContention }()

	var scenario windowScenario
	// windowEntered guards the window hook: the second participant's own
	// placement opens the same window, and the hook must not start a second
	// participant then.
	var windowEntered atomic.Bool
	oldWindow := blobPublishWindowHook
	blobPublishWindowHook = func(_, _ string) {
		if !windowEntered.CompareAndSwap(false, true) {
			return
		}
		// The window is open: the second participant tries to publish the
		// target right now.
		go func() {
			d, derr := blobDomainAcquire(t.Context(), blobPath, domainDst)
			if derr != nil {
				participantErrCh <- derr
				close(participantPublished)
				return
			}
			defer d.release()
			perr := blobPublishPlace(participantSrc, blobPath, d)
			// The participant records what the target holds right after its
			// own publication: if the publisher's rename replaces it, that
			// publication was destroyed.
			if data, rerr := os.ReadFile(blobPath); rerr == nil {
				scenario.participantPublishedBytes = data
			}
			participantErrCh <- perr
			close(participantPublished)
		}()
		// Wait for the outcome: either the ownership domain excluded the
		// second participant, or it published the target in the window.
		select {
		case <-participantWaiting:
			scenario.participantBlocked = true
		case <-participantPublished:
			scenario.participantPublishedInWindow = true
		case <-time.After(60 * time.Second):
			t.Error("the second participant neither was excluded by the ownership domain nor published")
		}
	}
	defer func() { blobPublishWindowHook = oldWindow }()

	scenario.publishErr = blobPublishPlace(publisherSrc, blobPath, domain)

	// The publisher's domain is released, so a participant that was excluded
	// from the window can finish.
	domain.release()

	select {
	case err := <-participantErrCh:
		scenario.participantErr = err
	case <-time.After(60 * time.Second):
		t.Fatal("the second participant never finished")
	}

	if data, rerr := os.ReadFile(blobPath); rerr == nil {
		scenario.targetExists = true
		scenario.published = data
	}
	return scenario
}

// checkDomainExclusiveWindowOutcome states the property the domain-exclusive
// tier must have and reports the first violation of it.
//
// It is a plain function so the negative control can assert that the SAME
// property FAILS under the pre-#140 model.
func checkDomainExclusiveWindowOutcome(out windowScenario) error {
	switch {
	case out.publishErr != nil:
		return fmt.Errorf("the placement failed: %w", out.publishErr)
	case !out.participantBlocked:
		return errors.New("the second domain participant was not excluded by the ownership domain")
	case out.participantPublishedInWindow:
		return errors.New("the second domain participant published the target inside the placement's window")
	case !out.targetExists:
		return errors.New("the placement published no blob")
	case !bytes.Equal(out.published, windowPublisherBytes):
		return fmt.Errorf("the published blob holds %q, want the publisher's bytes", out.published)
	case !errors.Is(out.participantErr, errBlobTargetExists):
		return fmt.Errorf("the second participant's placement = %v, want errBlobTargetExists", out.participantErr)
	}
	return nil
}

// TestDomainExclusivePublicationExcludesADomainParticipantFromItsWindow is
// the positive regression for the domain-exclusive tier's guarantee.
//
// The tier's placement renames over the target, and the rename REPLACES an
// existing target. The guarantee "an existing blobs/<sha> is never replaced
// by a DOMAIN PARTICIPANT" therefore rests on the exclusion the ownership
// domain provides, and this test pins that exclusion:
//
//   - the placement's window between its existence check and its rename is
//     open while the publisher holds the blob key's ownership domain;
//   - a second writer of the same blob key - a domain participant - is
//     EXCLUDED from that window, so it cannot create blobs/<sha> there;
//   - so the placement's no-replace outcome for that participant is
//     guaranteed rather than incidental: once the participant gets the
//     domain, its own placement refuses to replace the published blob and
//     the proven bytes survive.
//
// NOT guaranteed, and not claimed here: a writer OUTSIDE the domain (the
// mirror writer) is not fenced by the ownership domain and can still create
// the target in the window. That limitation stays documented.
func TestDomainExclusivePublicationExcludesADomainParticipantFromItsWindow(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	sha := strings.Repeat("9", 64)
	out := runPublicationWindowScenario(t, repo, sha)

	if err := checkDomainExclusiveWindowOutcome(out); err != nil {
		t.Fatalf("the domain-exclusive tier lost its window guarantee: %v", err)
	}
	// The excluded participant's payload is left for it to discard: the
	// refusal never consumed it.
	if _, serr := os.Stat(filepath.Join(repo.BlobsDir(), "tmp-"+sha+".other")); serr != nil {
		t.Errorf("the refused participant's payload was removed: %v", serr)
	}
}

// TestNegativeControlStatThenRenameWithoutASharedDomainReplacesTheWinner is
// the negative control for the property above.
//
// It reconstructs the pre-#140 model through the production seams:
//
//   - the placement is the historical Stat-then-rename, which REPLACES a
//     target that appeared between the stat and the rename;
//   - there is no shared ownership domain for the blob key, so the second
//     writer of that key is not excluded from the placement's window.
//
// The control proves that the positive regression really detects the defect:
// under the pre-#140 model the SAME property fails - the second participant
// publishes inside the window, and the publisher's rename then destroys the
// blob it published.
func TestNegativeControlStatThenRenameWithoutASharedDomainReplacesTheWinner(t *testing.T) {
	forceUnsupportedHardLinks(t)
	repo := newPublicationRepo(t)

	// The historical placement: stat the target, refuse when it exists,
	// otherwise rename over it. The historical window between the stat and
	// the rename is kept, so the same scenario can be replayed against it.
	oldPlace := blobPublishPlace
	blobPublishPlace = func(src, dst string, _ *blobDomain) error {
		if _, err := os.Stat(dst); err == nil {
			return errBlobTargetExists
		}
		if blobPublishWindowHook != nil {
			blobPublishWindowHook(src, dst)
		}
		return os.Rename(src, dst)
	}
	defer func() { blobPublishPlace = oldPlace }()

	// The historical ownership: every writer keeps its own private domain,
	// so the second writer of the blob key is not excluded from the window.
	oldAcquire := blobDomainAcquire
	blobDomainAcquire = func(context.Context, string, string) (*blobDomain, error) {
		return &blobDomain{}, nil
	}
	defer func() { blobDomainAcquire = oldAcquire }()

	sha := strings.Repeat("8", 64)
	out := runPublicationWindowScenario(t, repo, sha)

	// The positive property must FAIL under the pre-#140 model: that is
	// what makes the positive regression discriminating.
	if err := checkDomainExclusiveWindowOutcome(out); err == nil {
		t.Fatal("the pre-#140 model satisfied the domain-exclusive window property: the negative control did not reproduce the defect")
	}
	// The defect itself: the second participant published inside the window
	// and the publisher's rename replaced the blob it published.
	if out.participantBlocked {
		t.Fatal("the negative control still excluded the second participant from the window")
	}
	if !out.participantPublishedInWindow {
		t.Fatal("the negative control did not reproduce the defect: the second participant did not publish inside the window")
	}
	if !bytes.Equal(out.participantPublishedBytes, windowParticipantBytes) {
		t.Fatalf("the second participant's published blob holds %q, want its own bytes", out.participantPublishedBytes)
	}
	if !bytes.Equal(out.published, windowPublisherBytes) {
		t.Fatalf("the publisher's rename did not replace the second participant's blob: %q", out.published)
	}
	t.Logf("negative control reproduced the defect: the participant published %q inside the window and the publisher's rename replaced it with %q",
		out.participantPublishedBytes, out.published)
}
