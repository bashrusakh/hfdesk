// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

// withCoordinationAccount replaces the account resolution for one test. The
// injected account stands for the identity a numeric container runtime
// produces: a numeric account id and a home directory the container already
// sets, without any account record the name service could answer.
func withCoordinationAccount(t *testing.T, account coordinationAccount, err error) {
	t.Helper()
	previous := coordinationAccountProvider
	coordinationAccountProvider = func() (coordinationAccount, error) { return account, err }
	t.Cleanup(func() { coordinationAccountProvider = previous })
}

// TestCoordinationAccountResolvesNumericIdentityWithoutAccountRecord pins
// the account model of the private coordination carrier: the identity is the
// numeric account of the platform, the private base directory is the account's
// own directory, and no account record has to exist for either of them.
func TestCoordinationAccountResolvesNumericIdentityWithoutAccountRecord(t *testing.T) {
	home := t.TempDir()
	withCoordinationAccount(t, coordinationAccount{ID: "uid:4242", Home: home}, nil)
	restore := runtimecoordination.SetBaseDirProvider(nil)
	defer restore()

	base, err := defaultCoordinationBaseDir()
	if err != nil {
		t.Fatalf("defaultCoordinationBaseDir: %v", err)
	}
	digest := sha256.Sum256([]byte("uid:4242"))
	if want := "hfdesk-coord-" + hex.EncodeToString(digest[:]); filepath.Base(base) != want {
		t.Fatalf("coordination base %q does not name the account %q", base, want)
	}
	if !PathInside(home, base) {
		t.Fatalf("coordination base %q is not inside the account's own directory %q", base, home)
	}
	// The carrier stays outside the model and cache locations of that
	// account, so an ordinary write into them can never reach it.
	for _, other := range []string{
		filepath.Join(home, ".cache", "huggingface"),
		filepath.Join(home, "models"),
		filepath.Join(home, "Storage"),
	} {
		if PathInside(other, base) {
			t.Fatalf("coordination base %q is inside the model or cache path %q", base, other)
		}
	}
	// The same account always resolves the same carrier, so the app and the
	// CLI of one account meet on it.
	again, err := defaultCoordinationBaseDir()
	if err != nil {
		t.Fatalf("defaultCoordinationBaseDir: %v", err)
	}
	if again != base {
		t.Fatalf("the same account resolved two carriers: %q and %q", base, again)
	}
	// A second account on the same machine does not share the carrier.
	withCoordinationAccount(t, coordinationAccount{ID: "uid:4243", Home: home}, nil)
	other, err := defaultCoordinationBaseDir()
	if err != nil {
		t.Fatalf("defaultCoordinationBaseDir: %v", err)
	}
	if other == base {
		t.Fatalf("two accounts share one coordination carrier: %q", base)
	}
}

// TestDownloadSurvivesUnresolvableCoordinationAccount pins the availability
// rule of the coordination carrier: the cross-process lease is advisory, so
// an account the platform cannot name must not fail a download.
func TestDownloadSurvivesUnresolvableCoordinationAccount(t *testing.T) {
	withCoordinationAccount(t, coordinationAccount{}, errors.New("user: Current requires cgo or $USER set in environment"))
	restore := runtimecoordination.SetBaseDirProvider(nil)
	defer restore()

	body := []byte("payload")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: int64(len(body))}})
		case strings.HasSuffix(r.URL.Path, "/config.json"):
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cacheDir := t.TempDir()
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{
		Endpoint: server.URL, CacheDir: cacheDir, Concurrency: 1, MaxActiveDownloads: 1,
	}, nil); err != nil {
		t.Fatalf("download failed while the coordination account was unresolvable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "hub", "models--owner--model", "refs", "main")); err != nil {
		t.Fatalf("download did not materialize the repository: %v", err)
	}
	// The lease itself stays available: it keeps its in-process half instead
	// of refusing the operation.
	release, err := AcquireRebuildCoordinationLeases([]string{cacheDir}, true)
	if err != nil {
		t.Fatalf("coordination lease failed while the account was unresolvable: %v", err)
	}
	release()
}

// TestCoordinationCarrierAnotherAccountCanReachDoesNotFailTheOperation pins
// the denial-of-service rule of the per-account carrier: a directory another
// account can reach is refused, and the refusal degrades the coordination
// instead of failing the operation that asked for a lease.
func TestCoordinationCarrierAnotherAccountCanReachDoesNotFailTheOperation(t *testing.T) {
	base := filepath.Join(t.TempDir(), "coordination-base")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	restore := runtimecoordination.SetBaseDirProvider(func() (string, error) { return base, nil })
	defer restore()

	root := t.TempDir()
	release, err := AcquireRebuildCoordinationLeases([]string{root}, true)
	if err != nil {
		t.Fatalf("a carrier another account can reach failed the lease: %v", err)
	}
	release()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == coordinationDirectoryName {
			t.Fatal("the coordination directory was created inside a carrier another account can reach")
		}
	}
}

// TestCoordinationCarrierIsSharedBySameNumericAccountWithoutAccountRecord
// exercises the container shape of the account model in separate processes:
// a numeric account id without an account record, USER unset and HOME set.
// The app and the CLI of that account must resolve one carrier, so their
// mutual exclusion survives.
func TestCoordinationCarrierIsSharedBySameNumericAccountWithoutAccountRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the NSS-less numeric identity is a Unix shape")
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skipf("unshare is unavailable: %v", err)
	}
	probe := exec.Command(unshare, "--user", "--map-user=4242", "--map-group=4242", "id", "-u")
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("an unprivileged user namespace is unavailable: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	helper := buildRebuildLeaseHolder(t)
	home := t.TempDir()
	root := t.TempDir()

	resolve := func() string {
		t.Helper()
		command := exec.Command(unshare, "--user", "--map-user=4242", "--map-group=4242",
			"env", "-u", "USER", "HOME="+home, helper, "carrier-path", root)
		command.Env = rebuildTestEnvWithOverrides(map[string]string{
			"HOME": home, "USER": "", "HFDESK_TEST_COORDINATION_DIR": "",
		})
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("numeric account carrier probe failed: %v (%s)", err, strings.TrimSpace(string(output)))
		}
		line := strings.TrimSpace(string(output))
		if !strings.HasPrefix(line, "CARRIER_PATH\t") {
			t.Fatalf("the numeric account carrier probe printed %q", line)
		}
		carrier := strings.TrimSpace(strings.TrimPrefix(line, "CARRIER_PATH\t"))
		if carrier == "" {
			t.Fatal("the numeric account resolved no coordination carrier")
		}
		return carrier
	}
	first := resolve()
	if !strings.HasPrefix(first, home+string(filepath.Separator)) {
		t.Fatalf("carrier %q is not inside the account's own private area %q", first, home)
	}
	second := resolve()
	if first != second {
		t.Fatalf("the same numeric account resolved two carriers: %q and %q", first, second)
	}
}

// TestOrdinaryDownloadSkipsBlobAnotherTransferIsDownloading pins the
// behaviour of an ordinary download that meets a contended blob: it reports
// the contended blob as skipped instead of waiting for the other transfer.
func TestOrdinaryDownloadSkipsBlobAnotherTransferIsDownloading(t *testing.T) {
	body := []byte("shared-content-hash")
	digest := sha256.Sum256(body)
	hash := hex.EncodeToString(digest[:])
	var payloadRequested bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{{
				Type: "file", Path: "weights.bin", Size: int64(len(body)),
				LFS: &hfLfsInfo{Oid: hash, Size: int64(len(body))},
			}})
		case strings.HasSuffix(r.URL.Path, "/weights.bin"):
			payloadRequested = true
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cacheDir := t.TempDir()
	cache := NewHFCache(cacheDir, 0)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Another live transfer owns the blob: the incomplete file and its
	// metadata name this process, so the blob reads as downloading.
	if err := os.WriteFile(repo.IncompletePath(hash), body[:4], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteIncompleteMeta(hash, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if status, _, err := repo.CheckBlob(hash); err != nil || status != BlobDownloading {
		t.Fatalf("precondition CheckBlob=(%s,%v); want downloading", status, err)
	}

	var messages []string
	done := make(chan error, 1)
	go func() {
		done <- Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, Settings{
			Endpoint: server.URL, CacheDir: cacheDir, Concurrency: 1, MaxActiveDownloads: 1,
		}, func(event ProgressEvent) {
			if event.Event == "file_done" {
				messages = append(messages, event.Message)
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("download failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the ordinary download waited for the other transfer instead of skipping the contended blob")
	}
	if payloadRequested {
		t.Fatal("the ordinary download fetched the blob another transfer owns")
	}
	skip := false
	for _, message := range messages {
		if strings.Contains(message, "downloading by another process") {
			skip = true
		}
	}
	if !skip {
		t.Fatalf("the contended blob was not reported as skipped: %v", messages)
	}
}

// TestCoordinationCarrierProbeHelperIsUsable keeps the helper's carrier-path
// mode covered: it prints the carrier of one root, which the numeric-account
// probe above compares across processes.
func TestCoordinationCarrierProbeHelperIsUsable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the helper probe is a Unix shape")
	}
	helper := buildRebuildLeaseHolder(t)
	command := exec.Command(helper, "carrier-path", t.TempDir())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("carrier path probe failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	line := strings.TrimSpace(string(output))
	if !strings.HasPrefix(line, "CARRIER_PATH\t") {
		t.Fatalf("carrier path probe printed %q", line)
	}
	if strings.TrimSpace(strings.TrimPrefix(line, "CARRIER_PATH\t")) == "" {
		t.Fatal("carrier path probe printed no carrier")
	}
}
