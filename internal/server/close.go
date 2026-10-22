// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import "context"

// Close quiesces the job manager and closes it, releasing the resources the
// server allocated. It is the cleanup a host needs when startup fails before
// the server ever served a request, and it must be safe to call more than
// once.
//
// It performs the same sequence the graceful shutdown performs for the job
// manager: the manager is quiesced first, so that no new work is dispatched,
// and then it is closed with the caller's context. The HTTP server is not
// touched, because at this point there is none.
//
// The job manager runs its close once, so a second call waits for the same
// result instead of closing it again.
func (s *Server) Close(ctx context.Context) error {
	if s == nil || s.jobs == nil {
		return nil
	}
	s.jobs.Quiesce()
	return s.jobs.Close(ctx)
}
