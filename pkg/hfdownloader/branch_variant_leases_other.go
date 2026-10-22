// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !illumos

package hfdownloader

import (
	"context"
	"fmt"
)

func acquireBranchVariantOSDirectoryLock(context.Context, string, bool) (func(), error) {
	return nil, fmt.Errorf("cross-process branch variant directory leases are unsupported on this platform")
}
