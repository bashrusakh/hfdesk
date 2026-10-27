// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Tests same-key exclusion across the no-hard-link fallback's check/rename window.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	windowPublisherBytes   = []byte("the publisher's bytes")
	windowParticipantBytes = []byte("the other participant's bytes")
)

type windowScenario struct {
	publishErr                   error
	published                    []byte
	targetExists                 bool
	participantBlocked           bool
	participantPublishedInWindow bool
	participantErr               error
}

// runPublicationWindowScenario races a same-key writer against the fallback's
// check/rename hook and records the result.
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

	// Hold the blob key's domain throughout placement, as downloader publishers do.
	domain, err := blobDomainAcquire(t.Context(), blobPath, domainDst)
	if err != nil {
		t.Fatal(err)
	}
	defer domain.release()

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
	// The participant's placement invokes the same hook; start only one competitor.
	var windowEntered atomic.Bool
	oldWindow := blobPublishWindowHook
	blobPublishWindowHook = func(_, _ string) {
		if !windowEntered.CompareAndSwap(false, true) {
			return
		}
		go func() {
			d, derr := blobDomainAcquire(t.Context(), blobPath, domainDst)
			if derr != nil {
				participantErrCh <- derr
				close(participantPublished)
				return
			}
			defer d.release()
			perr := blobPublishPlace(participantSrc, blobPath, d)
			participantErrCh <- perr
			close(participantPublished)
		}()
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
