// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"fmt"
	"os"
	"os/exec"
)

func createCoordinationDirectoryAliasForTest(link, target string) error {
	if err := os.Symlink(target, link); err == nil {
		return nil
	}
	command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("create directory symlink/junction: %w: %s", err, output)
	}
	return nil
}
