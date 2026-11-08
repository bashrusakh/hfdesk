// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
)

// beginSettingsOperation admits one settings mutation before the handover seal.
func (s *Server) beginSettingsOperation() bool {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.settingsSealed {
		return false
	}
	s.settingsOps.Add(1)
	return true
}

// CompleteHandover drains admitted settings writes, checks the latest runtime
// generation's persisted outcome, and joins the existing job-manager close.
// It never initiates another settings write.
func (s *Server) CompleteHandover() error {
	if s == nil {
		return nil
	}
	s.handoverOnce.Do(func() {
		s.configMu.Lock()
		s.settingsSealed = true
		s.configMu.Unlock()

		// Admission is sealed; wait without holding config or persistence locks.
		s.settingsOps.Wait()

		var settingsErr error
		func() {
			s.persistMu.Lock()
			defer s.persistMu.Unlock()

			cfg, gen := s.snapshotConfigWithGen()
			if gen == 0 || (s.settingsPersistedGen == gen && cfg.tokenWrite == nil) {
				return
			}
			if s.settingsPersistErrGen == gen && s.settingsPersistErr != nil {
				settingsErr = fmt.Errorf("settings generation %d was not persisted: %w", gen, s.settingsPersistErr)
			} else {
				settingsErr = fmt.Errorf("settings generation %d was not confirmed persisted", gen)
			}
		}()

		// Close(ctx) may have returned on its shutdown budget while its close
		// coordinator still drains runners/history and writes the final paused
		// snapshot. Join that same coordinator with an independent context, and
		// preserve both persistence outcomes.
		var jobsErr error
		if s.jobs != nil {
			jobsErr = s.jobs.Close(context.Background())
		}
		s.handoverErr = errors.Join(settingsErr, jobsErr)
	})
	return s.handoverErr
}
