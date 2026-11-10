// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Content-addressed publication uses atomic hard-link no-replace when
// supported. Otherwise it checks and renames under the blob's ownership domain;
// that fallback excludes domain participants, not external writers such as the
// mirror. See doc.go for the complete guarantee and limitation.

import (
	"errors"
	"fmt"
	"log"
	"os"
)

// errBlobTargetExists means the existing blob is the completion proof.
var errBlobTargetExists = errors.New("content-addressed blob already exists")

// errBlobPublicationDomainRequired means fallback placement lacks the blob key's domain.
var errBlobPublicationDomainRequired = errors.New("publishing without replacing an existing blob needs the ownership domain of the blob key")

// blobLink is replaceable in tests to exercise the no-hard-link fallback.
var blobLink = os.Link

var blobPublishPlace = placeBlobNoReplace

// blobPublishWindowHook exposes the fallback's check/rename window to tests.
var blobPublishWindowHook func(src, dst string)

// placeBlobNoReplace places src at dst, returning errBlobTargetExists if dst
// exists. The fallback requires the ownership domain for dst's blob key.
func placeBlobNoReplace(src, dst string, dom *blobDomain) error {
	err := blobLink(src, dst)
	switch {
	case err == nil:
		if rmErr := os.Remove(src); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("warning: could not remove staging file %s after publishing %s: %v", src, dst, rmErr)
		}
		return nil
	case errors.Is(err, os.ErrExist):
		return errBlobTargetExists
	}
	if !dom.holdsBlobKey(dst) {
		return fmt.Errorf("publish %s: %w", dst, errBlobPublicationDomainRequired)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		return errBlobTargetExists
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	// The domain excludes its participants from this replacing-rename window.
	if blobPublishWindowHook != nil {
		blobPublishWindowHook(src, dst)
	}
	if renameErr := os.Rename(src, dst); renameErr != nil {
		return renameErr
	}
	return nil
}
