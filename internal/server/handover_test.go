// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func waitForSettingsSeal(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.configMu.RLock()
		sealed := s.settingsSealed
		s.configMu.RUnlock()
		if sealed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("handover did not seal settings admission")
		}
		time.Sleep(time.Millisecond)
	}
}

func newHandoverTestServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	path := isolateTokenConfig(t)
	return New(cfg), path
}

func TestCompleteHandoverFencesAndDrainsAdmittedSettings(t *testing.T) {
	s, _ := newHandoverTestServer(t, DefaultConfig())
	s.persistMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.persistMu.Unlock()
		}
	}()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- postTokenSettings(s, `{"token":"hf_admitted","connections":13}`) }()
	awaitTokenGeneration(t, s, 1)
	if !s.beginSettingsOperation() {
		t.Fatal("pre-Phase-2 update was not admitted")
	}
	phaseTwo, applied := make(chan struct{}), make(chan struct{})
	go func() {
		defer s.settingsOps.Done()
		<-phaseTwo
		s.withConfig(func(cfg *Config) { cfg.MaxActive = 17 })
		close(applied)
		s.persistMu.Lock()
		cfg, gen := s.snapshotConfigWithGen()
		_ = s.persistAdmittedSettings(cfg, gen)
		s.persistMu.Unlock()
	}()
	done := make(chan error, 1)
	go func() { done <- s.CompleteHandover() }()
	waitForSettingsSeal(t, s)
	select {
	case err := <-done:
		t.Fatalf("handover returned before admitted operations: %v", err)
	default:
	}
	if late := postTokenSettings(s, `{"connections":21}`); late.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-seal settings status = %d", late.Code)
	}
	if cfg, gen := s.snapshotConfigWithGen(); cfg.Concurrency != 13 || gen != 1 {
		t.Fatalf("post-seal mutation: connections=%d gen=%d", cfg.Concurrency, gen)
	}
	close(phaseTwo)
	<-applied
	s.persistMu.Unlock()
	locked = false
	if response := <-first; !strings.Contains(response.Body.String(), "skipped") {
		t.Fatalf("superseded admitted request = %s", response.Body.String())
	}
	err := <-done
	assertNoError(t, err, "")
	file, err := LoadConfigFile()
	if err != nil || file.Connections != 13 || file.MaxActive != 17 || file.Token != "hf_admitted" {
		t.Fatalf("latest admitted settings snapshot = %+v, %v", file, err)
	}
}

func TestCompleteHandoverDoesNotAdmitAnUnvalidatedBody(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Token = "hf_runtime_only"
	s, path := newHandoverTestServer(t, cfg)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.handleUpdateSettings(w, httptest.NewRequest(http.MethodPost, "/api/settings", reader))
		response <- w
	}()
	_, err := io.WriteString(writer, `{"connections":`)
	assertNoError(t, err, "")
	err = s.CompleteHandover()
	assertNoError(t, err, "handover waited for body validation: %v", err)
	_, _ = io.WriteString(writer, `19}`)
	_ = writer.Close()
	if w := <-response; w.Code != http.StatusServiceUnavailable {
		t.Fatalf("late body status = %d, want 503", w.Code)
	}
	if cfg, gen := s.snapshotConfigWithGen(); cfg.Concurrency != DefaultConfig().Concurrency || gen != 0 {
		t.Fatalf("unvalidated body mutated config: connections=%d generation=%d", cfg.Concurrency, gen)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("generation-zero startup override was written: %v", err)
	}
}

// TestCompleteHandoverBoundsAStalledJobsPersistence proves the join of the
// job-manager close coordinator has a budget: a final jobs-state write that
// never completes must not block the handover forever, or Quit would never
// finish, the desktop singleton would refuse every later launch, and the
// elevated helper would abandon the attempt after its own budget. The stalled
// attempt completes inside the budget with an error, which the desktop keeps
// as the cause of the refused handover instead of signaling success.
func TestCompleteHandoverBoundsAStalledJobsPersistence(t *testing.T) {
	s, _ := newHandoverTestServer(t, DefaultConfig())

	const budget = 50 * time.Millisecond
	previousBudget := handoverJoinBudget
	handoverJoinBudget = budget
	t.Cleanup(func() { handoverJoinBudget = previousBudget })

	stall := make(chan struct{})
	// Release the stalled final write and join the close coordinator before
	// the TempDir cleanup runs: an abandoned finishClose must not outlive the
	// directory it would otherwise write into.
	t.Cleanup(func() {
		close(stall)
		<-s.jobs.closeDone
	})
	s.jobs.persistStateFile = func(string, []*Job) error {
		<-stall
		return nil
	}

	start := time.Now()
	err := s.CompleteHandover()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a stalled jobs persistence was reported as a completed handover")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("handover error = %v, want the deadline of the bounded join", err)
	}
	if !strings.Contains(err.Error(), "final jobs state was not persisted") {
		t.Errorf("the error %q does not name the stalled persistence", err)
	}
	if elapsed > time.Second {
		t.Errorf("CompleteHandover returned after %s, want the %s join budget", elapsed, budget)
	}
	// The handover is sealed once: a repeated call reports the same outcome
	// instead of joining the stalled write again.
	if repeated := s.CompleteHandover(); repeated == nil || !errors.Is(repeated, context.DeadlineExceeded) {
		t.Errorf("repeated handover result = %v, want the same deadline error", repeated)
	}
}

// TestCompleteHandoverBoundsAStalledAdmittedSettingsWrite proves the drain of
// admitted settings writes has a budget: a write that never finishes (a
// stalled disk or network mount) must not block the handover forever, or Quit
// would never finish, the desktop singleton would refuse every later launch,
// and the elevated helper would abandon the attempt after its own budget. The
// stalled drain ends inside the budget with an error, which the desktop keeps
// as the cause of the refused handover instead of signaling success: no
// service is installed or started for persistence that did not complete.
func TestCompleteHandoverBoundsAStalledAdmittedSettingsWrite(t *testing.T) {
	s, _ := newHandoverTestServer(t, DefaultConfig())

	const budget = 50 * time.Millisecond
	previousBudget := handoverJoinBudget
	handoverJoinBudget = budget
	t.Cleanup(func() { handoverJoinBudget = previousBudget })

	// A settings mutation admitted before the seal that never finishes, like
	// a write stuck on the storage: it releases only when the test ends.
	if !s.beginSettingsOperation() {
		t.Fatal("the settings mutation was not admitted before the seal")
	}
	released := false
	release := func() {
		if !released {
			released = true
			s.settingsOps.Done()
		}
	}
	t.Cleanup(release)
	// The bounded drain abandons the jobs close coordinator: Close returned
	// on the expired join context while finishClose still writes the final
	// jobs state into this test's TempDir. Release the admitted write and
	// join that coordinator before the TempDir cleanup runs, or its
	// MkdirAll/write races RemoveAll ("directory not empty"). Close joins an
	// already-running coordinator and triggers the spawn when the drain never
	// reached it, so every exit path leaves no writer behind.
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.jobs.Close(ctx); err != nil {
			t.Errorf("join the abandoned jobs close coordinator: %v", err)
			<-s.jobs.closeDone
		}
	})

	done := make(chan error, 1)
	go func() { done <- s.CompleteHandover() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stalled admitted settings write was reported as a completed handover")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("handover error = %v, want the deadline of the bounded drain", err)
		}
		if !strings.Contains(err.Error(), "admitted settings writes did not finish") {
			t.Errorf("the error %q does not name the stalled settings drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CompleteHandover did not return within 5s while an admitted settings write is still pending: the drain waits without a budget")
	}

	// The handover is sealed once: a repeated call reports the same outcome
	// instead of draining again.
	release()
	if repeated := s.CompleteHandover(); repeated == nil ||
		!errors.Is(repeated, context.DeadlineExceeded) ||
		!strings.Contains(repeated.Error(), "admitted settings writes did not finish") {
		t.Errorf("repeated handover result = %v, want the same drain deadline error", repeated)
	}
}

func TestCompleteHandoverSettingsGenerationOutcomes(t *testing.T) {
	for _, mode := range []string{"repair-obsolete", "latest-error"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newHandoverTestServer(t, DefaultConfig())
			writeErr := errors.New("settings write failed")
			var saves int
			realSave := s.saveSettingsConfigFn
			s.saveSettingsConfigFn = func(file *ConfigFile, token *string) error {
				saves++
				if saves == 1 {
					return writeErr
				}
				return realSave(file, token)
			}

			switch mode {
			case "repair-obsolete":
				if r := postTokenSettings(s, `{"token":"hf_pending","connections":4}`); !strings.Contains(r.Body.String(), "warning") {
					t.Fatalf("settings failure not reported: %s", r.Body.String())
				}
				if strings.Contains(postTokenSettings(s, `{"connections":8}`).Body.String(), "warning") {
					t.Fatal("latest settings write did not repair the old failure")
				}
			case "latest-error":
				s.persistMu.Lock()
				a, b := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
				go func() { a <- postTokenSettings(s, `{"token":"hf_pending","connections":4}`) }()
				awaitTokenGeneration(t, s, 1)
				go func() { b <- postTokenSettings(s, `{"connections":8}`) }()
				awaitTokenGeneration(t, s, 2)
				s.persistMu.Unlock()
				if x, y := <-a, <-b; !strings.Contains(x.Body.String(), "skipped") || !strings.Contains(y.Body.String(), "warning") {
					t.Fatalf("obsolete/latest writer results = %s / %s", x.Body.String(), y.Body.String())
				}
			}

			err := s.CompleteHandover()
			if mode == "latest-error" {
				if !errors.Is(err, writeErr) || s.snapshotConfig().tokenWrite == nil || s.settingsPersistedGen != 0 {
					t.Fatalf("latest failure result = %v", err)
				}
				select {
				case <-s.jobs.closeDone:
				default:
					t.Fatal("settings failure skipped the job close join")
				}
			} else {
				file, loadErr := LoadConfigFile()
				if err != nil || loadErr != nil || file.Connections != 8 || file.Token != "hf_pending" || s.settingsPersistedGen != 2 {
					t.Fatalf("repaired result = %v; file=%+v %v", err, file, loadErr)
				}
			}
			if repeated := s.CompleteHandover(); (mode == "latest-error" && !errors.Is(repeated, writeErr)) || (mode == "repair-obsolete" && repeated != nil) {
				t.Fatalf("repeated handover result = %v", repeated)
			}
			if saves != map[string]int{"repair-obsolete": 2, "latest-error": 1}[mode] {
				t.Fatalf("unexpected settings save count: %d", saves)
			}
		})
	}
}

// assertNoError fails the test when the call it wraps reported a failure,
// with the diagnostic the inline check printed.
func assertNoError(t *testing.T, err error, format string, a ...any) {
	t.Helper()
	if err == nil {
		return
	}
	if format == "" {
		t.Fatal(err)
		return
	}
	if len(a) == 0 {
		t.Fatal(format)
		return
	}
	t.Fatalf(format, a...)
}

// assertNames proves that a message names every fragment of the standard
// the test pins, one diagnostic per missing fragment.
func assertNames(t *testing.T, message, context string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(message, want) {
			t.Errorf("%s = %q, want a message that mentions %q", context, message, want)
		}
	}
}

// assertNamesWithout proves the named fragments AND the absent ones: the
// message keeps its standard and offers no second route.
func assertNamesWithout(t *testing.T, message, context string, wants, forbidden []string) {
	t.Helper()
	assertNames(t, message, context, wants)
	for _, unwanted := range forbidden {
		if strings.Contains(message, unwanted) {
			t.Errorf("%s = %q, must not mention %q", context, message, unwanted)
		}
	}
}
