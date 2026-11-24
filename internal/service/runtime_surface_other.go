// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package service

// openHostServiceManager stays nil on a platform whose service host has no
// settled registration scope.
//
// A user scope and a machine-wide scope exist on Linux and macOS, and the
// scope of an instance is not part of its run vector: reading the wrong one
// could report "not-installed" for a registration that is in place, which is
// a claim no read proved. The surface therefore stays unregistered there and
// the page keeps the unknown state, which never claims an absence.
var openHostServiceManager func() (Manager, error)
