// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
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
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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
	if _, err := io.WriteString(writer, `{"connections":`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteHandover(); err != nil {
		t.Fatalf("handover waited for body validation: %v", err)
	}
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
