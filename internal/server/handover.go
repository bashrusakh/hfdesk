// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// handoverJoinBudget bounds the whole join of admitted work after the
// admission seal: the drain of the admitted settings writes, the confirmation
// of the latest persisted settings generation, and the join of the
// job-manager close coordinator (Close may have returned on its own shutdown
// budget while its coordinator still drains runners/history and writes the
// final paused snapshot).
//
// One deadline covers the three so the join gets the size of one other
// shutdown phase and the desktop's completion stays inside its graceful
// shutdown plus this budget — well inside the 60 s the elevated helper waits
// for the completion evidence, so an ordinary handover still finishes inside
// the helper's window. The bound is what keeps Quit finishable: an unbounded
// wait would block CompleteHandover forever when an admitted settings write
// or the final write stalls, the desktop would never exit, the singleton
// would refuse every later launch, and the helper would abandon the attempt
// after its own budget anyway. A write that does not finish inside the budget
// is reported as an error, never as a completed handover, so the desktop does
// not authorize the service installation for persistence that did not
// complete.
//
// It is a variable so the tests can shorten it.
var handoverJoinBudget = 30 * time.Second

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

// CompleteHandover drains admitted settings writes within the join budget,
// checks the latest runtime generation's persisted outcome, and joins the
// existing job-manager close on the same deadline. It never initiates another
// settings write.
func (s *Server) CompleteHandover() error {
	if s == nil {
		return nil
	}
	s.handoverOnce.Do(func() {
		s.configMu.Lock()
		s.settingsSealed = true
		s.configMu.Unlock()

		// One deadline bounds the whole join below: the settings drain and
		// the jobs close share it, so their sum can never leave the window
		// the elevated helper waits in.
		ctx, cancel := context.WithTimeout(context.Background(), handoverJoinBudget)
		defer cancel()

		// Admission is sealed; wait without holding config or persistence
		// locks. The wait runs on its own goroutine so the deadline can
		// abandon an admitted write that never finishes; that goroutine ends
		// when the write itself ends. A stalled write holds persistMu (the
		// persistence runs under it), so a drained-with-timeout join skips
		// the generation check instead of blocking on the lock the stalled
		// write still owns: the drain error already carries the settings
		// outcome.
		drained := make(chan struct{})
		go func() {
			s.settingsOps.Wait()
			close(drained)
		}()
		var drainErr error
		select {
		case <-drained:
		case <-ctx.Done():
			drainErr = fmt.Errorf("the admitted settings writes did not finish within %s: %w",
				handoverJoinBudget, ctx.Err())
		}

		var settingsErr error
		if drainErr == nil {
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
		}

		// Close(ctx) may have returned on its shutdown budget while its close
		// coordinator still drains runners/history and writes the final paused
		// snapshot. Join that same coordinator on the remaining budget, and
		// preserve both persistence outcomes: a write that does not finish
		// inside the budget is reported as an error, never as a completed
		// handover, so the desktop does not authorize the service
		// installation for persistence that did not complete.
		var jobsErr error
		if s.jobs != nil {
			jobsErr = s.jobs.Close(ctx)
			if errors.Is(jobsErr, context.DeadlineExceeded) {
				jobsErr = fmt.Errorf("the final jobs state was not persisted within %s: %w",
					handoverJoinBudget, jobsErr)
			}
		}
		s.handoverErr = errors.Join(drainErr, settingsErr, jobsErr)
	})
	return s.handoverErr
}
