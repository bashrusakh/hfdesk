// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The desktop name of a release build the fixtures are written for: the
// workflow publishes hfdesk_windows_amd64_<tag>.exe beside
// hfdesk-desktop_windows_amd64_<tag>.exe, so the stamp in the name is the
// release tag.
const (
	testReleaseDesktop    = "hfdesk-desktop_windows_amd64_v1.3.0-beta.7.exe"
	testReleaseAsset      = "hfdesk_windows_amd64_v1.3.0-beta.7.exe"
	testReleaseTag        = "v1.3.0-beta.7"
	testReleaseAssetSize  = 12772352
	testLocalBuildDesktop = "hfdesk-desktop.exe"
)

// sha256Hex returns the lowercase hex SHA-256 of a payload, the way a
// release digest is spelled.
func sha256Hex(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// serveRelease starts a local release server and points the release lookup
// at it for the duration of the test: the API base and the only allowed
// download prefix become the test server's, so nothing can reach the real
// network and a fixture can prove the prefix guard as well.
func serveRelease(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)

	previousAPI := githubAPIBaseURL
	previousPrefix := releaseDownloadAllowedPrefix
	githubAPIBaseURL = server.URL
	releaseDownloadAllowedPrefix = server.URL + "/"
	t.Cleanup(func() {
		githubAPIBaseURL = previousAPI
		releaseDownloadAllowedPrefix = previousPrefix
		server.Close()
	})
	return server
}

// releaseFixtureJSON builds the metadata answer of the release API for one
// paired asset: its size, its digest (empty when the fixture publishes none)
// and its download URL inside the test server.
func releaseFixtureJSON(server *httptest.Server, payload, digest string, withChecksums bool) string {
	assets := []consoleReleaseAsset{{
		Name:               testReleaseAsset,
		Size:               int64(len(payload)),
		Digest:             digest,
		BrowserDownloadURL: server.URL + "/download/" + testReleaseAsset,
	}}
	if withChecksums {
		assets = append(assets, consoleReleaseAsset{
			Name:               releaseChecksumsAssetName,
			BrowserDownloadURL: server.URL + "/download/" + releaseChecksumsAssetName,
		})
	}
	body, err := json.Marshal(githubReleaseResponse{Assets: assets})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// releaseMux serves the release API, the asset download and the checksum
// list of one fixture payload; payloadHandler, when set, serves the asset
// request itself (a test that wants a broken or cancelled transfer uses it).
func releaseMux(t *testing.T, server **httptest.Server, payload, digest string, withChecksums bool,
	payloadHandler http.HandlerFunc) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(releaseFixtureJSON(*server, payload, digest, withChecksums)))
	})
	mux.HandleFunc("/download/"+testReleaseAsset, func(w http.ResponseWriter, r *http.Request) {
		if payloadHandler != nil {
			payloadHandler(w, r)
			return
		}
		_, _ = w.Write([]byte(payload))
	})
	mux.HandleFunc("/download/"+releaseChecksumsAssetName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sha256Hex(payload) + "  " + testReleaseAsset + "\n"))
	})
	return mux
}

// folderEntries returns the names left in a folder, so a test can prove a
// failure left nothing behind and a success added exactly one file.
func folderEntries(t *testing.T, folder string) []string {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatalf("the folder %s cannot be read: %v", folder, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestConsoleReleaseForDerivesTheReleaseOfTheVersionStamp proves the release
// tag and the asset name come from the desktop program's own name alone: the
// stamp is the tag, the asset is the pair, and a name without a version — a
// local build — names no release and is never offered a download.
func TestConsoleReleaseForDerivesTheReleaseOfTheVersionStamp(t *testing.T) {
	for _, tc := range []struct {
		name      string
		desktop   string
		wantTag   string
		wantAsset string
		wantOK    bool
	}{
		{
			name:      "the release tag inside the file name",
			desktop:   testReleaseDesktop,
			wantTag:   testReleaseTag,
			wantAsset: testReleaseAsset,
			wantOK:    true,
		},
		{
			name:      "a stamp without the v prefix gets the tag spelling",
			desktop:   "hfdesk-desktop_windows_amd64_1.4.0.exe",
			wantTag:   "v1.4.0",
			wantAsset: "hfdesk_windows_amd64_1.4.0.exe",
			wantOK:    true,
		},
		{
			name:      "a path in front of the name changes nothing",
			desktop:   filepath.Join(`C:\portable`, testReleaseDesktop),
			wantTag:   testReleaseTag,
			wantAsset: testReleaseAsset,
			wantOK:    true,
		},
		{name: "a local build", desktop: testLocalBuildDesktop, wantOK: false},
		{name: "a name with only the platform", desktop: "hfdesk-desktop_windows_amd64.exe", wantOK: false},
		{name: "a renamed program", desktop: "launcher.exe", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release, ok := consoleReleaseFor(tc.desktop)
			if ok != tc.wantOK {
				t.Fatalf("consoleReleaseFor(%q) reported %t, want %t", tc.desktop, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if release.Tag != tc.wantTag {
				t.Errorf("the tag is %q, want %q", release.Tag, tc.wantTag)
			}
			if release.Asset != tc.wantAsset {
				t.Errorf("the asset is %q, want %q", release.Asset, tc.wantAsset)
			}
		})
	}
}

// TestResolveConsoleDownloadOfferNeedsAnExactAssetAndAChecksum proves the
// offer exists only where the download can be verified: the exact paired
// asset of this release's own tag, published from the official release
// downloads, with an authoritative SHA-256 source — and never for a local
// build, which reaches no server at all.
func TestResolveConsoleDownloadOfferNeedsAnExactAssetAndAChecksum(t *testing.T) {
	payload := "console-program-payload"
	digest := "sha256:" + sha256Hex(payload)

	t.Run("the API digest of the exact asset", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(releaseFixtureJSON(server, payload, digest, false)))
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil {
			t.Fatalf("the offer failed: %v", err)
		}
		if offer == nil {
			t.Fatal("no offer was resolved although the release publishes the digest")
		}
		if offer.Release.Tag != testReleaseTag || offer.Release.Asset != testReleaseAsset {
			t.Errorf("the offer names release %q asset %q, want %q and %q",
				offer.Release.Tag, offer.Release.Asset, testReleaseTag, testReleaseAsset)
		}
		if offer.Size != int64(len(payload)) {
			t.Errorf("the size is %d, want %d", offer.Size, len(payload))
		}
		if offer.Digest != digest {
			t.Errorf("the digest is %q, want %q", offer.Digest, digest)
		}
		if offer.URL == "" || !allowedReleaseDownloadURL(offer.URL) {
			t.Errorf("the URL %q is not inside the official release downloads", offer.URL)
		}
	})

	t.Run("the checksum list when the API carries no digest", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixtureJSON(server, payload, "", true)))
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want the checksum list as the source", offer, err)
		}
		if offer.Digest != "" {
			t.Errorf("the digest is %q, want none", offer.Digest)
		}
		if offer.ChecksumsURL == "" || !allowedReleaseDownloadURL(offer.ChecksumsURL) {
			t.Errorf("the checksum list %q is not inside the official release downloads", offer.ChecksumsURL)
		}
	})

	t.Run("no checksum source means no offer", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixtureJSON(server, payload, "", false)))
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if offer != nil {
			t.Fatalf("the offer exists without any checksum source: %v", offer)
		}
		if err == nil || !strings.Contains(err.Error(), "no SHA-256 checksum") {
			t.Errorf("the error is %v, want it to name the missing checksum", err)
		}
	})

	t.Run("a digest of another algorithm is no source", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixtureJSON(server, payload, "sha512:abcdef", false)))
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if offer != nil {
			t.Fatalf("the offer trusts a digest of another algorithm: %v", offer)
		}
		if err == nil || !strings.Contains(err.Error(), "no SHA-256 checksum") {
			t.Errorf("the error is %v, want it to name the missing checksum", err)
		}
	})

	t.Run("a release that does not publish the pair", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(githubReleaseResponse{Assets: []consoleReleaseAsset{{
				Name:               "hfdesk_linux_amd64_" + testReleaseTag,
				Size:               7,
				Digest:             digest,
				BrowserDownloadURL: server.URL + "/download/other",
			}}})
			_, _ = w.Write(body)
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if offer != nil {
			t.Fatalf("the offer points at an asset the release does not publish: %v", offer)
		}
		if err == nil || !strings.Contains(err.Error(), "does not publish "+testReleaseAsset) {
			t.Errorf("the error is %v, want it to name the missing asset", err)
		}
	})

	t.Run("a download URL outside the official releases", func(t *testing.T) {
		serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(githubReleaseResponse{Assets: []consoleReleaseAsset{{
				Name:               testReleaseAsset,
				Size:               int64(len(payload)),
				Digest:             digest,
				BrowserDownloadURL: "https://evil.example/" + testReleaseAsset,
			}}})
			_, _ = w.Write(body)
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if offer != nil {
			t.Fatalf("the offer points outside the official release downloads: %v", offer)
		}
		if err == nil || !strings.Contains(err.Error(), "official release downloads") {
			t.Errorf("the error is %v, want it to name the official downloads", err)
		}
	})

	t.Run("an unreadable release", func(t *testing.T) {
		serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}))

		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if offer != nil {
			t.Fatalf("the offer exists although the release cannot be read: %v", offer)
		}
		if err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("the error is %v, want it to name the failed read", err)
		}
	})

	t.Run("a local build reaches no server", func(t *testing.T) {
		requests := 0
		serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			http.Error(w, "must not be asked", http.StatusInternalServerError)
		}))

		offer, err := resolveConsoleDownloadOffer(testLocalBuildDesktop)
		if offer != nil || err != nil {
			t.Fatalf("the local build got (%v, %v), want no offer and no error", offer, err)
		}
		if requests != 0 {
			t.Errorf("the local build caused %d requests, want none", requests)
		}
	})

	t.Run("a case-renamed desktop resolves the same offer", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixtureJSON(server, payload, digest, false)))
		}))

		// The local pair lookup matches names case-insensitively —
		// the rule of the Windows file systems — so the release
		// metadata must not narrow it: the pair of this case-renamed
		// program is the published asset, and the offer still follows
		// it to the URL the release reports.
		const renamed = "HFDesk-Desktop_Windows_AMD64_v1.3.0-beta.7.exe"
		offer, err := resolveConsoleDownloadOffer(renamed)
		if err != nil {
			t.Fatalf("the offer failed: %v", err)
		}
		if offer == nil {
			t.Fatal("no offer was resolved for the case-renamed desktop although the release publishes its pair")
		}
		if want := "hfdesk_Windows_AMD64_v1.3.0-beta.7.exe"; offer.Release.Asset != want {
			t.Errorf("the offer names asset %q, want the pair of the renamed program %q", offer.Release.Asset, want)
		}
		if offer.Release.Tag != testReleaseTag {
			t.Errorf("the tag is %q, want %q", offer.Release.Tag, testReleaseTag)
		}
		if offer.URL == "" || !allowedReleaseDownloadURL(offer.URL) {
			t.Errorf("the URL %q is not inside the official release downloads", offer.URL)
		}
	})

	t.Run("a case-renamed desktop takes its line of the checksum list", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, "", true, nil))

		const renamed = "HFDesk-Desktop_Windows_AMD64_v1.3.0-beta.7.exe"
		offer, err := resolveConsoleDownloadOffer(renamed)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want the checksum list as the source", offer, err)
		}

		// The whole path has to hold together: the offer's derived
		// pair name, the checksum line that names the published
		// asset in its own case, and the transfer that verifies
		// against that line before it places anything.
		folder := t.TempDir()
		outcome, err := downloadConsoleRelease(context.Background(), offer, folder, nil)
		if err != nil {
			t.Fatalf("the download of the renamed pair failed: %v", err)
		}
		if !outcome.Placed {
			t.Error("the downloaded pair is not marked as placed by this transfer")
		}
		if entries := folderEntries(t, folder); len(entries) != 1 || entries[0] != filepath.Base(outcome.Path) {
			t.Errorf("the folder holds %v, want only the downloaded program", entries)
		}
	})
}

// TestDownloadConsoleReleaseVerifiesBeforeItPublishes proves the happy path:
// the exact asset lands under its release name in the folder of the desktop
// app, and nothing else does — the staging file of the transfer is gone
// because the placement consumes it, not because it was left behind.
func TestDownloadConsoleReleaseVerifiesBeforeItPublishes(t *testing.T) {
	payload := "console-program-payload"
	digest := "sha256:" + sha256Hex(payload)
	var server *httptest.Server
	server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))

	folder := t.TempDir()
	offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
	if err != nil || offer == nil {
		t.Fatalf("the offer is (%v, %v), want one", offer, err)
	}

	var reported []int64
	outcome, err := downloadConsoleRelease(context.Background(), offer, folder,
		func(downloaded, total int64) { reported = append(reported, downloaded) })
	if err != nil {
		t.Fatalf("the download failed: %v", err)
	}
	want := filepath.Join(folder, testReleaseAsset)
	if outcome.Path != want {
		t.Errorf("the path is %q, want %q", outcome.Path, want)
	}
	if !outcome.Placed {
		t.Error("the published file is not marked as placed by this transfer, so a later cancel could not remove it")
	}
	content, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("the downloaded program cannot be read: %v", err)
	}
	if string(content) != payload {
		t.Errorf("the file holds %q, want the payload %q", content, payload)
	}
	if entries := folderEntries(t, folder); len(entries) != 1 || entries[0] != testReleaseAsset {
		t.Errorf("the folder holds %v, want only %s", entries, testReleaseAsset)
	}
	if len(reported) == 0 {
		t.Error("the transfer reported no progress")
	}
	if last := reported[len(reported)-1]; last != int64(len(payload)) {
		t.Errorf("the last progress report is %d bytes, want %d", last, len(payload))
	}
}

// TestDownloadConsoleReleaseAcceptsTheChecksumList proves the second
// authoritative source works the same way when the API carries no digest for
// the asset.
func TestDownloadConsoleReleaseAcceptsTheChecksumList(t *testing.T) {
	payload := "console-program-payload"
	var server *httptest.Server
	server = serveRelease(t, releaseMux(t, &server, payload, "", true, nil))

	folder := t.TempDir()
	offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
	if err != nil || offer == nil {
		t.Fatalf("the offer is (%v, %v), want one", offer, err)
	}

	outcome, err := downloadConsoleRelease(context.Background(), offer, folder, nil)
	if err != nil {
		t.Fatalf("the download failed: %v", err)
	}
	if entries := folderEntries(t, folder); len(entries) != 1 || entries[0] != filepath.Base(outcome.Path) {
		t.Errorf("the folder holds %v, want only the downloaded program", entries)
	}
}

// TestDownloadConsoleReleaseLeavesNoFileOnEveryFailure proves the failure
// modes the offer promises: a checksum mismatch, a release without an
// authoritative checksum, a broken answer of the server and an unwritable
// folder all end without a single file at the destination.
func TestDownloadConsoleReleaseLeavesNoFileOnEveryFailure(t *testing.T) {
	payload := "console-program-payload"
	digest := "sha256:" + sha256Hex(payload)

	t.Run("a checksum mismatch", func(t *testing.T) {
		// The wrong body stays within the advertised size, so it is
		// the verification and not the write bound that rejects it —
		// this fixture pins the digest check specifically.
		const wrongPayload = "a-payload-of-another"
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, digest, false,
			func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(wrongPayload))
			}))

		folder := t.TempDir()
		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want one", offer, err)
		}

		_, err = downloadConsoleRelease(context.Background(), offer, folder, nil)
		if err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("the error is %v, want the verification failure", err)
		}
		if !strings.Contains(err.Error(), sha256Hex(wrongPayload)) ||
			!strings.Contains(err.Error(), sha256Hex(payload)) {
			t.Errorf("the error %q does not state both digests", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty", entries)
		}
	})

	t.Run("no checksum source fails closed before any file", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, "", false, nil))
		folder := t.TempDir()
		offer := &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    int64(len(payload)),
			URL:     server.URL + "/download/" + testReleaseAsset,
		}

		_, err := downloadConsoleRelease(context.Background(), offer, folder, nil)
		if err == nil || !strings.Contains(err.Error(), "no SHA-256 checksum") {
			t.Fatalf("the error is %v, want the missing checksum", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty — a file without a checksum must never exist", entries)
		}
	})

	t.Run("a checksum list without the asset", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, releaseChecksumsAssetName):
				_, _ = w.Write([]byte(sha256Hex(payload) + "  some-other-file.exe\n"))
			case strings.HasSuffix(r.URL.Path, testReleaseAsset):
				_, _ = w.Write([]byte(payload))
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(releaseFixtureJSON(server, payload, "", true)))
			}
		}))

		folder := t.TempDir()
		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want one", offer, err)
		}

		if _, err := downloadConsoleRelease(context.Background(), offer, folder, nil); err == nil ||
			!strings.Contains(err.Error(), "no SHA-256 checksum") {
			t.Fatalf("the error is %v, want the missing checksum", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty", entries)
		}
	})

	t.Run("a broken answer of the release server", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, digest, false,
			func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "broken", http.StatusInternalServerError)
			}))

		folder := t.TempDir()
		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want one", offer, err)
		}

		if _, err := downloadConsoleRelease(context.Background(), offer, folder, nil); err == nil ||
			!strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("the error is %v, want the status of the answer", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty", entries)
		}
	})

	t.Run("an unwritable folder", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("a root process writes into a read-only folder")
		}
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))

		folder := t.TempDir()
		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want one", offer, err)
		}
		if err := os.Chmod(folder, 0o555); err != nil {
			t.Fatalf("the folder cannot be made read-only: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(folder, 0o755) })

		if _, err := downloadConsoleRelease(context.Background(), offer, folder, nil); err == nil ||
			!strings.Contains(err.Error(), "cannot be written") {
			t.Fatalf("the error is %v, want the refused write", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty", entries)
		}
	})

	t.Run("a URL outside the official release downloads", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))
		folder := t.TempDir()
		offer := &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    int64(len(payload)),
			URL:     "https://evil.example/" + testReleaseAsset,
		}

		_, err := downloadConsoleRelease(context.Background(), offer, folder, nil)
		if err == nil || !strings.Contains(err.Error(), "official release downloads") {
			t.Fatalf("the error is %v, want the refused host", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty", entries)
		}
	})

	t.Run("more bytes than the release publishes fail without a file", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))
		folder := t.TempDir()
		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want one", offer, err)
		}
		// The bound the write trusts comes from the release metadata;
		// a body that sends more than that advertised size must fail
		// the download instead of filling the disk until the digest
		// check could reject it.
		offer.Size = int64(len(payload)) - 1

		_, err = downloadConsoleRelease(context.Background(), offer, folder, nil)
		if err == nil || !strings.Contains(err.Error(), "more than") {
			t.Fatalf("the over-length download reported %v, want the refused bound", err)
		}
		if !strings.Contains(err.Error(), consoleSizeText(offer.Size)) {
			t.Errorf("the error %q does not name the bound the body exceeded", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty — over-length data must leave no file", entries)
		}
	})

	t.Run("an offer without a size cannot be bounded and fails before any file", func(t *testing.T) {
		var server *httptest.Server
		server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))
		folder := t.TempDir()
		offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
		if err != nil || offer == nil {
			t.Fatalf("the offer is (%v, %v), want one", offer, err)
		}
		offer.Size = 0

		_, err = downloadConsoleRelease(context.Background(), offer, folder, nil)
		if err == nil || !strings.Contains(err.Error(), "cannot be bounded") {
			t.Fatalf("the unbounded offer reported %v, want the refused bound", err)
		}
		if entries := folderEntries(t, folder); len(entries) != 0 {
			t.Errorf("the folder holds %v, want it empty — an offer without a size must never create a file", entries)
		}
	})
}

// TestDownloadConsoleReleaseCancelLeavesNoFile proves a cancelled transfer
// ends through the context — the report names the cancellation and the
// staging file is gone before the call returns, so the dialog that reports
// it can state what is really left.
func TestDownloadConsoleReleaseCancelLeavesNoFile(t *testing.T) {
	payload := "console-program-payload"
	digest := "sha256:" + sha256Hex(payload)
	var server *httptest.Server
	// The server sends a first piece of the payload and then holds the
	// response open, the way a stalled link does, until the client goes
	// away.
	server = serveRelease(t, releaseMux(t, &server, payload, digest, false,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("partial"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		}))

	folder := t.TempDir()
	offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
	if err != nil || offer == nil {
		t.Fatalf("the offer is (%v, %v), want one", offer, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := false
	_, err = downloadConsoleRelease(ctx, offer, folder, func(downloaded, total int64) {
		if !cancelled {
			cancelled = true
			cancel()
		}
	})
	if !cancelled {
		t.Fatal("the transfer finished before the test could cancel it")
	}
	if !errors.Is(err, errConsoleDownloadCanceled) {
		t.Errorf("the error is %v, want the cancellation", err)
	}
	if entries := folderEntries(t, folder); len(entries) != 0 {
		t.Errorf("the folder holds %v, want it empty after a cancel", entries)
	}
}

// TestDownloadConsoleReleaseKeepsAFileThatAppearedDuringTheTransfer proves
// the publish never replaces a file it did not create: when the pair shows
// up while the bytes are in flight, the download discards its own copy and
// the action continues with the file that is there.
func TestDownloadConsoleReleaseKeepsAFileThatAppearedDuringTheTransfer(t *testing.T) {
	payload := "console-program-payload"
	digest := "sha256:" + sha256Hex(payload)
	var server *httptest.Server
	server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))

	folder := t.TempDir()
	offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
	if err != nil || offer == nil {
		t.Fatalf("the offer is (%v, %v), want one", offer, err)
	}
	existing := filepath.Join(folder, testReleaseAsset)
	if err := os.WriteFile(existing, []byte("hand-placed"), 0o644); err != nil {
		t.Fatalf("the pre-existing program cannot be written: %v", err)
	}

	outcome, err := downloadConsoleRelease(context.Background(), offer, folder, nil)
	if err != nil {
		t.Fatalf("the download failed although the file is there: %v", err)
	}
	if outcome.Path != existing {
		t.Errorf("the path is %q, want the file that is already there %q", outcome.Path, existing)
	}
	if outcome.Placed {
		t.Error("the file that was there is marked as placed by this transfer, so a later cancel could delete it")
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("the program cannot be read: %v", err)
	}
	if string(content) != "hand-placed" {
		t.Errorf("the file that was there was replaced with %q", content)
	}
	if entries := folderEntries(t, folder); len(entries) != 1 {
		t.Errorf("the folder holds %v, want only the file that was there", entries)
	}
}

// TestDownloadConsoleReleaseKeepsAFileCreatedAfterTheDestinationCheck
// proves the placement itself never replaces a file it did not place: a
// program that appears after the destination check reported the name free —
// the pair a user handplaces while the verified bytes wait — survives the
// placement with its content, is not marked as placed by this transfer, and
// still survives the user's cancel of the finished transfer. A placement
// that decided from the check and then renamed would silently take the
// name over in exactly this window, record the foreign file as its own and
// remove it on that later cancel.
func TestDownloadConsoleReleaseKeepsAFileCreatedAfterTheDestinationCheck(t *testing.T) {
	payload := "console-program-payload"
	digest := "sha256:" + sha256Hex(payload)
	var server *httptest.Server
	server = serveRelease(t, releaseMux(t, &server, payload, digest, false, nil))

	folder := t.TempDir()
	offer, err := resolveConsoleDownloadOffer(testReleaseDesktop)
	if err != nil || offer == nil {
		t.Fatalf("the offer is (%v, %v), want one", offer, err)
	}

	// The file appears in the window between the destination check and the
	// placement: the seam runs exactly there, after the check reported the
	// name free and before the verified file is placed.
	existing := filepath.Join(folder, testReleaseAsset)
	previous := consoleDestinationChecked
	consoleDestinationChecked = func(target string) {
		if err := os.WriteFile(target, []byte("hand-placed"), 0o644); err != nil {
			t.Errorf("the interleaved program cannot be written: %v", err)
		}
	}
	t.Cleanup(func() { consoleDestinationChecked = previous })

	outcome, err := downloadConsoleRelease(context.Background(), offer, folder, nil)
	if err != nil {
		t.Fatalf("the download failed although the file appeared only after its check: %v", err)
	}
	if outcome.Path != existing {
		t.Errorf("the path is %q, want the file that took the name %q", outcome.Path, existing)
	}
	if outcome.Placed {
		t.Error("the interleaved file is marked as placed by this transfer, so a later cancel could delete it")
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("the interleaved program cannot be read: %v", err)
	}
	if string(content) != "hand-placed" {
		t.Errorf("the placement replaced the interleaved file with %q", content)
	}
	if entries := folderEntries(t, folder); len(entries) != 1 || entries[0] != testReleaseAsset {
		t.Errorf("the folder holds %v, want only the file that took the name", entries)
	}

	// The user's cancel in the window after the transfer finished must
	// keep the file as well: it was never this transfer's to remove.
	if path, settleErr := settleConsoleDownload(true, nil, outcome, nil); path != "" ||
		!errors.Is(settleErr, errConsoleDownloadCanceled) {
		t.Fatalf("the settlement is (%q, %v), want the cancellation with no file", path, settleErr)
	}
	if content, err := os.ReadFile(existing); err != nil || string(content) != "hand-placed" {
		t.Errorf("the cancel removed the interleaved file: (%q, %v), want it kept", content, err)
	}
}

// TestSHA256FromChecksums proves the parse of the release checksum list
// takes exactly the line of the asset — the binary marker of a Windows list
// and line endings aside — and never a line of another file or a malformed
// hash.
func TestSHA256FromChecksums(t *testing.T) {
	asset := testReleaseAsset
	hash := sha256Hex("payload")
	for _, tc := range []struct {
		name string
		list string
		want string
		ok   bool
	}{
		{name: "the line of the asset", list: hash + "  " + asset + "\n", want: hash, ok: true},
		{name: "with the binary marker", list: hash + " *" + asset + "\r\n", want: hash, ok: true},
		{name: "among other lines", list: "deadbeef  other.exe\n" + hash + "  " + asset, want: hash, ok: true},
		{name: "the same name in another case", list: hash + "  HFDESK_Windows_AMD64_V1.3.0-BETA.7.EXE\n", want: hash, ok: true},
		{name: "another file's line", list: hash + "  other.exe\n", ok: false},
		{name: "a malformed hash", list: "not-a-hash  " + asset + "\n", ok: false},
		{name: "an empty list", list: "", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sha256FromChecksums(tc.list, asset)
			if ok != tc.ok {
				t.Fatalf("sha256FromChecksums reported %t, want %t", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("the digest is %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConsoleSizeTextRendersTheNoteSize proves the size the note line shows
// is the release size in the units a user reads a program size in.
func TestConsoleSizeTextRendersTheNoteSize(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want string
	}{
		{size: 0, want: "0 bytes"},
		{size: 812, want: "812 bytes"},
		{size: 1024, want: "1.0 KB"},
		{size: 1536, want: "1.5 KB"},
		{size: testReleaseAssetSize, want: "12.2 MB"},
	} {
		if got := consoleSizeText(tc.size); got != tc.want {
			t.Errorf("consoleSizeText(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}

// TestSettleConsoleDownloadSeparatesTheUserCancelFromTheProgrammaticClose
// pins both directions of the progress dialog's closing semantic: the
// user's Cancel — including one in the window after the transfer already
// finished and verified — removes the file this transfer placed and
// reports the cancellation, so the service action does not continue; the
// dialog's own programmatic close after a finished transfer is no cancel
// and hands the verified file to the caller. A file the transfer did not
// place keeps its protection in both directions, and a dialog failure
// decides nothing about the user.
func TestSettleConsoleDownloadSeparatesTheUserCancelFromTheProgrammaticClose(t *testing.T) {
	placedFile := func(t *testing.T) consoleDownloadOutcome {
		t.Helper()
		folder := t.TempDir()
		target := filepath.Join(folder, testReleaseAsset)
		if err := os.WriteFile(target, []byte("verified-payload"), 0o644); err != nil {
			t.Fatalf("the verified program cannot be written: %v", err)
		}
		return consoleDownloadOutcome{Path: target, Placed: true}
	}

	t.Run("the user's cancel after completion removes the placed file", func(t *testing.T) {
		outcome := placedFile(t)
		path, err := settleConsoleDownload(true, nil, outcome, nil)
		if !errors.Is(err, errConsoleDownloadCanceled) {
			t.Fatalf("the settlement is (%q, %v), want the cancellation with its cause", path, err)
		}
		if path != "" {
			t.Errorf("the cancelled settlement continues with %q, want no file", path)
		}
		if entries := folderEntries(t, filepath.Dir(outcome.Path)); len(entries) != 0 {
			t.Errorf("the folder holds %v, want the file this transfer placed removed", entries)
		}
	})

	t.Run("the programmatic close continues with the verified file", func(t *testing.T) {
		outcome := placedFile(t)
		path, err := settleConsoleDownload(false, nil, outcome, nil)
		if err != nil {
			t.Fatalf("the programmatic close reported %v, want the verified file to continue", err)
		}
		if path != outcome.Path {
			t.Errorf("the settlement continues with %q, want %q", path, outcome.Path)
		}
		if entries := folderEntries(t, filepath.Dir(outcome.Path)); len(entries) != 1 {
			t.Errorf("the folder holds %v, want the verified file kept", entries)
		}
	})

	t.Run("the cancel keeps a file the transfer did not place", func(t *testing.T) {
		folder := t.TempDir()
		target := filepath.Join(folder, testReleaseAsset)
		if err := os.WriteFile(target, []byte("hand-placed"), 0o644); err != nil {
			t.Fatalf("the hand-placed program cannot be written: %v", err)
		}
		_, err := settleConsoleDownload(true, nil, consoleDownloadOutcome{Path: target}, nil)
		if !errors.Is(err, errConsoleDownloadCanceled) {
			t.Fatalf("the settlement reported %v, want the cancellation", err)
		}
		content, readErr := os.ReadFile(target)
		if readErr != nil || string(content) != "hand-placed" {
			t.Errorf("the file the transfer did not place is (%q, %v), want it kept", content, readErr)
		}
	})

	t.Run("the cancel of a running transfer keeps its own cause", func(t *testing.T) {
		path, err := settleConsoleDownload(true, nil, consoleDownloadOutcome{}, errConsoleDownloadCanceled)
		if !errors.Is(err, errConsoleDownloadCanceled) || path != "" {
			t.Errorf("the settlement is (%q, %v), want no file and the cancellation", path, err)
		}
	})

	t.Run("a failed transfer under the programmatic close reports its own error", func(t *testing.T) {
		cause := errors.New("the download did not finish: connection reset")
		path, err := settleConsoleDownload(false, nil, consoleDownloadOutcome{}, cause)
		if path != "" || !errors.Is(err, cause) {
			t.Errorf("the settlement is (%q, %v), want no file and the failure of the transfer", path, err)
		}
	})

	t.Run("a dialog failure is no cancel of the user", func(t *testing.T) {
		outcome := placedFile(t)
		dialogErr := errors.New("the dialog could not be shown (HRESULT 0x800704c7)")
		path, err := settleConsoleDownload(false, dialogErr, outcome, nil)
		if err != nil || path != outcome.Path {
			t.Errorf("the settlement is (%q, %v), want the verified file to continue although the dialog failed",
				path, err)
		}
		path, err = settleConsoleDownload(false, dialogErr, consoleDownloadOutcome{}, errConsoleDownloadCanceled)
		if path != "" || err == nil || !strings.Contains(err.Error(), "progress dialog could not be shown") {
			t.Errorf("the settlement is (%q, %v), want the dialog failure as the cause of the cancelled transfer",
				path, err)
		}
	})
}

// TestConsoleDialogCloseReadsTheProgrammaticCloseAsNoCancel proves how the
// end of the progress dialog is attributed before it reaches the
// settlement: a dialog that closed without the programmatic close was
// ended by the user — the Cancel press, Esc or the close button — the
// announced programmatic close of a finished transfer is nobody's cancel,
// and a dialog whose run failed was answered by no one.
func TestConsoleDialogCloseReadsTheProgrammaticCloseAsNoCancel(t *testing.T) {
	var user consoleDialogClose
	if !user.userCancelled(nil) {
		t.Error("a dialog that closed without the programmatic close was not read as the user's cancel")
	}

	var programmatic consoleDialogClose
	programmatic.transferSettled()
	if programmatic.userCancelled(nil) {
		t.Error("the programmatic close of a finished transfer was read as a cancel of the user")
	}

	var failed consoleDialogClose
	if failed.userCancelled(errors.New("HRESULT 0x800704c7")) {
		t.Error("a dialog whose run failed was read as a cancel of the user")
	}
}
