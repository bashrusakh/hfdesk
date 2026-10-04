// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolateTokenConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"XDG_CONFIG_HOME", "AppData", "USERPROFILE", "HOME"} {
		t.Setenv(key, dir)
	}
	t.Chdir(dir)
	path := ConfigPath()
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Fatalf("config path outside isolated directory: %q", path)
	}
	return path
}

func postTokenSettings(s *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleUpdateSettings(w, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))
	return w
}

func requireTokenSettingsOK(t *testing.T, s *Server, body string) string {
	t.Helper()
	w := postTokenSettings(s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("settings status %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func requireTokenState(t *testing.T, s *Server, live, disk string) {
	t.Helper()
	if s.snapshotConfig().Token != live || s.jobs.snapshotConfig().Token != live {
		t.Error("live server/job-manager token does not match intended credential")
	}
	file, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if file.Token != disk {
		t.Error("stored token does not match intended credential")
	}
}

func TestSettingsTokenPreservation(t *testing.T) {
	for _, initial := range []struct{ name, live, disk string }{
		{"none", "", ""},
		{"file", "hf_file_credential", "hf_file_credential"},
		{"flag-only", "hf_runtime_credential", ""},
		{"flag-over-file", "hf_runtime_credential", "hf_file_credential"},
	} {
		t.Run(initial.name, func(t *testing.T) {
			isolateTokenConfig(t)
			if err := SaveConfigFile(&ConfigFile{Token: initial.disk}); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.Token = initial.live
			cfg.ModelsDir, cfg.DatasetsDir = "models-fixed", "datasets-fixed"
			s := New(cfg)
			for _, tokenField := range []string{"", `,"token":null`, `,"token":"********"`, `,"token":"********stale"`} {
				for range 2 {
					requireTokenSettingsOK(t, s, `{"connections":3,"maxActive":2,"retries":0,"maxSpeed":"2MB","cacheDir":"cache-updated"`+tokenField+`}`)
					requireTokenState(t, s, initial.live, initial.disk)
				}
			}
			live, jobs := s.snapshotConfig(), s.jobs.snapshotConfig()
			file, err := LoadConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			if live.Concurrency != 3 || jobs.Concurrency != 3 || file.Connections != 3 || live.MaxActive != 2 || jobs.MaxActive != 2 || file.MaxActive != 2 || file.Retries == nil || *file.Retries != 0 || file.MaxSpeed != "2MB" || file.CacheDir != "cache-updated" || live.ModelsDir != "models-fixed" || jobs.DatasetsDir != "datasets-fixed" {
				t.Error("unrelated settings behavior changed")
			}
		})
	}
}

// An explicit token set is a one-shot persistence intent: once it has been
// durably written, a later ordinary settings save must preserve whatever is
// stored on disk at that time. Before the fix tokenWrite was never cleared, so
// the first explicit set became a process-lifetime override that clobbered an
// out-of-band edit on every subsequent save.
func TestSettingsTokenOutOfBandPreservedOnOrdinarySave(t *testing.T) {
	path := isolateTokenConfig(t)
	if err := SaveConfigFile(&ConfigFile{Token: "hf_disk_seed"}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Token = "hf_runtime_override"
	s := New(cfg)

	// Explicit set: live and stored become A.
	requireTokenSettingsOK(t, s, `{"token":"hf_authored_A"}`)
	requireTokenState(t, s, "hf_authored_A", "hf_authored_A")

	// Out-of-band change to the selected config file (not via the API):
	// the stored token is now B while the live token stays A.
	writeTokenFixture(t, path, `{"token":"hf_out_of_band_B"}`)
	if stored, err := LoadConfigFile(); err != nil || stored.Token != "hf_out_of_band_B" {
		t.Fatalf("out-of-band seed failed: token=%q err=%v", stored.Token, err)
	}

	// Ordinary save with token omitted must preserve B, not re-apply A.
	requireTokenSettingsOK(t, s, `{"connections":12}`)
	if live := s.snapshotConfig().Token; live != "hf_authored_A" {
		t.Errorf("live token = %q, want %q", live, "hf_authored_A")
	}
	stored, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token != "hf_out_of_band_B" {
		t.Errorf("stored token = %q, want %q (ordinary save must not re-apply the earlier explicit set)", stored.Token, "hf_out_of_band_B")
	}
}

// TestSettingsTokenInheritedIntentNotReapplied targets the consume-identity
// contract directly. It models the exact interleaving where an explicit set is
// authored, a newer ordinary update inherits the pending intent and commits a
// new config generation, the explicit value is persisted and consumed, an
// out-of-band edit then changes the disk token, and finally the inherited
// ordinary snapshot is persisted. Because an ordinary update only inherits the
// intent (it authors no new tokenWriteGen), consuming by config generation
// would leave the intent live and the inherited persister would re-apply the
// already-persisted value, clobbering the out-of-band edit. The phases are
// driven directly through withConfig/saveSettingsConfig/consumeTokenWrite
// because the full-HTTP ordering cannot be scheduled deterministically (both
// requests block on persistMu).
func TestSettingsTokenInheritedIntentNotReapplied(t *testing.T) {
	path := isolateTokenConfig(t)
	if err := SaveConfigFile(&ConfigFile{Token: "hf_disk_seed"}); err != nil {
		t.Fatal(err)
	}
	s := New(DefaultConfig())

	// Request #1: explicit set A authors a fresh intent identity and snapshots
	// the config it just produced (generation 1).
	cfgExplicit, genExplicit := s.withConfig(func(c *Config) {
		token := "hf_authored_A"
		c.Token = token
		c.tokenWrite = &token
		c.tokenWriteGen++
	})
	if cfgExplicit.tokenWrite == nil {
		t.Fatal("explicit update did not author a pending intent")
	}

	// Request #2: ordinary update inherits the pending intent and bumps the
	// config generation (2) without authoring a new intent. This is the "newer
	// ordinary generation" from the bug report; it blocks on persistMu until #1
	// finishes.
	cfgOrdinary, genOrdinary := s.withConfig(func(c *Config) {
		c.Concurrency = 12
	})
	if genOrdinary <= genExplicit {
		t.Fatalf("ordinary generation %d did not advance past explicit generation %d", genOrdinary, genExplicit)
	}
	if cfgOrdinary.tokenWrite == nil || cfgOrdinary.tokenWriteGen != cfgExplicit.tokenWriteGen {
		t.Fatal("ordinary update did not inherit the pending intent identity")
	}

	// Request #1 wins persistMu: it persists A from its own snapshot, then
	// consumes the exact intent identity it carried. A config-generation
	// comparison would wrongly see generation 2 here and skip the clear.
	if err := saveSettingsConfig(&ConfigFile{Connections: cfgExplicit.Concurrency}, cfgExplicit.tokenWrite); err != nil {
		t.Fatal(err)
	}
	s.consumeTokenWrite(cfgExplicit.tokenWriteGen)

	// Out-of-band change to B. The intent is now durably satisfied, so a later
	// ordinary save must preserve this value.
	writeTokenFixture(t, path, `{"token":"hf_out_of_band_B"}`)
	if stored, err := LoadConfigFile(); err != nil || stored.Token != "hf_out_of_band_B" {
		t.Fatalf("out-of-band seed failed: token=%q err=%v", stored.Token, err)
	}

	// Request #2 now acquires persistMu and snapshots as the handler does,
	// after #1's consume. With the identity fix this snapshot carries no intent
	// and falls back to preserving the disk token; with a config-generation
	// consume the intent would still be live and the save below would clobber B.
	snap2, cur2 := s.snapshotConfigWithGen()
	if cur2 != genOrdinary {
		t.Fatalf("ordinary snapshot generation = %d, want %d", cur2, genOrdinary)
	}
	if err := saveSettingsConfig(&ConfigFile{Connections: snap2.Concurrency}, snap2.tokenWrite); err != nil {
		t.Fatal(err)
	}
	s.consumeTokenWrite(snap2.tokenWriteGen)

	stored, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token != "hf_out_of_band_B" {
		t.Errorf("stored token = %q, want %q (inherited ordinary snapshot must not re-apply the persisted explicit value)", stored.Token, "hf_out_of_band_B")
	}
	if live := s.snapshotConfig(); live.Token != "hf_authored_A" || live.Concurrency != 12 {
		t.Errorf("live token/concurrency = %q/%d, want %q/12 (in-memory state must be unchanged)", live.Token, live.Concurrency, "hf_authored_A")
	}
	if gen := s.snapshotConfig().tokenWriteGen; gen != 1 {
		t.Errorf("tokenWriteGen = %d, want unchanged 1", gen)
	}
}

func TestSettingsTokenExplicitSetClear(t *testing.T) {
	isolateTokenConfig(t)
	if err := SaveConfigFile(&ConfigFile{Token: "hf_disk_original"}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Token = "hf_runtime_original"
	s := New(cfg)
	for _, token := range []string{cfg.Token, "hf_authored_replacement", "", "hf_authored_after_clear"} {
		body, err := json.Marshal(map[string]string{"token": token})
		if err != nil {
			t.Fatal(err)
		}
		requireTokenSettingsOK(t, s, string(body))
		requireTokenState(t, s, token, token)
		for _, body := range []string{`{}`, `{"token":null}`, `{"token":"********old!"}`} {
			requireTokenSettingsOK(t, s, body)
			requireTokenState(t, s, token, token)
		}
	}
	w := postTokenSettings(s, `{"token":"hf_rejected","maxSpeed":"invalid"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("validation status = %d", w.Code)
	}
	requireTokenState(t, s, "hf_authored_after_clear", "hf_authored_after_clear")
}

func TestSettingsTokenRedaction(t *testing.T) {
	isolateTokenConfig(t)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	for _, token := range []string{"", "x", "xy", "xyz", "abcd", "hf_long_credential"} {
		cfg := DefaultConfig()
		cfg.Token = token
		s := New(cfg)
		w := httptest.NewRecorder()
		s.loggingMiddleware(http.HandlerFunc(s.handleGetSettings)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
		var resp SettingsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		want := ""
		if token != "" {
			want = "********"
			if len(token) > 4 {
				want += token[len(token)-4:]
			}
		}
		if resp.Token != want {
			t.Errorf("token of length %d not safely redacted", len(token))
		}
		body, err := json.Marshal(map[string]string{"token": resp.Token})
		if err != nil {
			t.Fatal(err)
		}
		requireTokenSettingsOK(t, s, string(body))
		if token != "" {
			requireTokenState(t, s, token, "")
		}
	}
	if strings.Contains(logs.String(), "hf_long_credential") || strings.Contains(logs.String(), "abcd") {
		t.Error("settings logs expose a credential")
	}
}

// Holding persistMu gates side effects; observing the generation under the
// config lock proves each request committed before the next request starts.
func awaitTokenGeneration(t *testing.T, s *Server, want uint64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		_, gen := s.snapshotConfigWithGen()
		if gen == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("generation did not reach %d", want)
		default:
			runtime.Gosched()
		}
	}
}

func TestSettingsTokenSkippedGeneration(t *testing.T) {
	for _, tc := range []struct{ name, firstToken, secondField, want string }{
		{"set-then-omitted", "hf_explicit_set", "", "hf_explicit_set"},
		{"clear-then-omitted", "", "", ""},
		{"set-then-null", "hf_explicit_set", `,"token":null`, "hf_explicit_set"},
		{"set-then-stale", "hf_explicit_set", `,"token":"********stale"`, "hf_explicit_set"},
		{"clear-then-stale", "", `,"token":"********stale"`, ""},
		{"set-then-clear", "hf_explicit_set", `,"token":""`, ""},
		{"clear-then-set", "", `,"token":"hf_newer_set"`, "hf_newer_set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTokenConfig(t)
			if err := SaveConfigFile(&ConfigFile{Token: "hf_previous_disk"}); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.Token = "hf_runtime_override"
			s := New(cfg)
			s.persistMu.Lock()
			locked := true
			defer func() {
				if locked {
					s.persistMu.Unlock()
				}
			}()
			first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
			body, err := json.Marshal(map[string]string{"token": tc.firstToken})
			if err != nil {
				t.Fatal(err)
			}
			go func() { first <- postTokenSettings(s, string(body)) }()
			awaitTokenGeneration(t, s, 1)
			go func() { second <- postTokenSettings(s, `{"connections":5`+tc.secondField+`}`) }()
			awaitTokenGeneration(t, s, 2)
			s.persistMu.Unlock()
			locked = false
			a, b := <-first, <-second
			if a.Code != http.StatusOK || !strings.Contains(a.Body.String(), "skipped") || b.Code != http.StatusOK || strings.Contains(b.Body.String(), "warning") {
				t.Fatalf("unexpected generation results: %s / %s", a.Body, b.Body)
			}
			requireTokenState(t, s, tc.want, tc.want)
			if s.jobs.snapshotConfig().Concurrency != 5 {
				t.Error("latest unrelated settings not applied")
			}
			requireTokenSettingsOK(t, s, `{}`)
			requireTokenState(t, s, tc.want, tc.want)
		})
	}
}

func TestSettingsTokenConcurrentExplicitWrites(t *testing.T) {
	isolateTokenConfig(t)
	s := New(DefaultConfig())
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"token":"hf_authored_%d","connections":%d}`, i, i+1)
			w := postTokenSettings(s, body)
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "warning") {
				t.Errorf("concurrent settings update failed: %s", w.Body)
			}
		}(i)
	}
	wg.Wait()
	live := s.snapshotConfig()
	if live.Concurrency < 1 || live.Concurrency > 12 || live.Token != fmt.Sprintf("hf_authored_%d", live.Concurrency-1) {
		t.Error("latest committed token/settings pair does not match any authored request")
	}
	requireTokenState(t, s, live.Token, live.Token)
	file, err := LoadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if file.Connections != live.Concurrency || s.jobs.snapshotConfig().Concurrency != live.Concurrency {
		t.Error("concurrent unrelated settings did not converge")
	}
}
