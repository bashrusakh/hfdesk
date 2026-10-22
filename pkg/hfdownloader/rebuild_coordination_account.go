// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"fmt"
)

// errCoordinationAccountUnavailable marks the case where the platform cannot
// name the account that owns the private coordination carrier.
//
// The carrier is an advisory cross-process mechanism, not a prerequisite of
// the operation that asks for a lease, so this condition never fails the
// operation: the caller degrades the coordination to the in-process half.
var errCoordinationAccountUnavailable = errors.New("HFDesk coordination account is unavailable")

// coordinationAccount names the OS account that owns the private coordination
// carrier.
//
// ID is the platform-native account identity. It is identical for every
// process of the same account and different for two accounts on one machine,
// so the carrier derived from it is shared by the app and the CLI of one
// account and is never shared between two accounts.
//
// Home is the account's private base directory. It is empty when the platform
// cannot name one.
type coordinationAccount struct {
	ID   string
	Home string
}

// coordinationAccountProvider resolves the account that owns the carrier. It
// is a package-level seam so a test can inject the resolution a numeric
// container identity produces, including a failing one.
var coordinationAccountProvider = currentCoordinationAccount

// currentCoordinationAccount resolves the account of the current process from
// the platform's own primitives: the numeric account identity that every
// platform can report without a name-service lookup, and the private base
// directory of that account.
func currentCoordinationAccount() (coordinationAccount, error) {
	id, err := coordinationOSUserID()
	if err != nil {
		return coordinationAccount{}, err
	}
	home, err := coordinationHomeDir()
	if err != nil {
		return coordinationAccount{}, err
	}
	return coordinationAccount{ID: id, Home: home}, nil
}

// resolveCoordinationAccount returns the account that owns the carrier.
//
// The resolution never performs an NSS lookup that a numeric container
// identity cannot satisfy: the platform's own numeric account identity is
// always available, and the private base directory comes from the account
// record when the platform has one and from the environment otherwise.
func resolveCoordinationAccount() (coordinationAccount, error) {
	account, err := coordinationAccountProvider()
	if err != nil {
		return coordinationAccount{}, fmt.Errorf("%w: %w", errCoordinationAccountUnavailable, err)
	}
	if account.ID == "" {
		return coordinationAccount{}, fmt.Errorf("%w: the platform named no account identity", errCoordinationAccountUnavailable)
	}
	if account.Home == "" {
		return coordinationAccount{}, fmt.Errorf("%w: the platform named no private base directory for account %q", errCoordinationAccountUnavailable, account.ID)
	}
	return account, nil
}
