// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// handoverJobsJoinBudget bounds the join of the job-manager close
// coordinator.
//
// Close may have returned on its own shutdown budget while its coordinator
// still drains runners/history and writes the final paused snapshot. The join
// gives that write the same size budget the other shutdown phases get (the
// desktop host bounds its graceful shutdown with 30 s), and it stays well
// inside the 60 s the elevated helper waits for the completion evidence, so an
// ordinary handover still finishes inside the helper's window. The bound is
// what keeps Quit finishable: an unbounded join would block CompleteHandover
// forever when the final write stalls, the desktop would never exit, the
// singleton would refuse every later launch, and the helper would abandon the
// attempt after its own budget anyway.
//
// It is a variable so the tests can shorten it.
var handoverJobsJoinBudget = 30 * time.Second

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
		// snapshot. Join that same coordinator with an independent, bounded
		// context, and preserve both persistence outcomes: a write that does
		// not finish inside the budget is reported as an error, never as a
		// completed handover, so the desktop does not authorize the service
		// installation for persistence that did not complete.
		var jobsErr error
		if s.jobs != nil {
			ctx, cancel := context.WithTimeout(context.Background(), handoverJobsJoinBudget)
			jobsErr = s.jobs.Close(ctx)
			cancel()
			if errors.Is(jobsErr, context.DeadlineExceeded) {
				jobsErr = fmt.Errorf("the final jobs state was not persisted within %s: %w",
					handoverJobsJoinBudget, jobsErr)
			}
		}
		s.handoverErr = errors.Join(settingsErr, jobsErr)
	})
	return s.handoverErr
}
