// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The official release endpoints of this repository.
//
// The missing console program is fetched from the releases of this repository
// alone: the repository is fixed here, the release is addressed by the tag of
// the desktop program's own name — never by "latest" — and every download URL
// has to stay inside the release download path of that repository, so no API
// answer can point the transfer at a file outside it.
const (
	releaseRepository         = "bashrusakh/hfdesk"
	releaseDownloadPrefix     = "https://github.com/" + releaseRepository + "/releases/download/"
	releaseTagPagePrefix      = "https://github.com/" + releaseRepository + "/releases/tag/"
	releaseChecksumsAssetName = "checksums.txt"
)

// The network facts of the release lookup, as variables so a test can serve
// them from a local server.
var (
	// githubAPIBaseURL is the base of the GitHub API that serves the
	// metadata of a release (assets, sizes, digests).
	githubAPIBaseURL = "https://api.github.com"
	// releaseDownloadAllowedPrefix is the only URL prefix the transfer may
	// read the program from; tests point it at their local server.
	releaseDownloadAllowedPrefix = releaseDownloadPrefix
	// consoleHTTPClient has no deadline of its own: every request is
	// bound by the context of its caller, so Cancel stops the transfer
	// instead of a clock that a slow link would race.
	consoleHTTPClient = &http.Client{}
	// consoleReleaseTimeout bounds the metadata read that decides whether
	// the notice can offer a verified download. It runs before the dialog
	// opens, so it has to end quickly on a machine without a network.
	consoleReleaseTimeout = 15 * time.Second
)

// consoleRelease names the one release asset a version-stamped desktop
// program's missing pair is published as: the release tag and the exact
// asset file name of the paired console program.
type consoleRelease struct {
	Tag   string
	Asset string
}

// consoleReleaseFor returns the release the paired console program of
// desktop is published in, and false when the program name carries no
// release version — a local build names no release, so it is never offered a
// download.
//
// The stamp inside the file name is the release tag (the workflow builds
// hfdesk_<os>_<arch>_<tag>.exe beside hfdesk-desktop_<os>_<arch>_<tag>.exe),
// normalized to the v-prefixed spelling this repository tags with. The asset
// is the paired name, so another version or another file can never be
// selected: only the pair of this very name.
func consoleReleaseFor(desktop string) (consoleRelease, bool) {
	stamp, versioned := desktopVersionStamp(desktop)
	paired := pairedConsoleExecutableName(desktop)
	if !versioned || paired == "" {
		return consoleRelease{}, false
	}
	return consoleRelease{
		Tag:   "v" + strings.TrimPrefix(stamp, "v"),
		Asset: paired,
	}, true
}

// consoleDownloadOffer is everything the notice and the transfer need about
// the paired asset of one release: where it is published, how large it is,
// and the authoritative SHA-256 sources of that same release.
type consoleDownloadOffer struct {
	Release consoleRelease
	Size    int64
	// URL is the release download URL of the exact asset, as the release
	// metadata of this repository reports it.
	URL string
	// Digest is the SHA-256 the API publishes for the asset
	// ("sha256:<hex>"), or empty when the API carries none.
	Digest string
	// ChecksumsURL is the release download URL of checksums.txt, the
	// checksum list of the whole release, or empty when it is not
	// published.
	ChecksumsURL string
}

// consoleReleaseAsset is one asset of a release as the GitHub API returns it.
type consoleReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// githubReleaseResponse is the part of the release metadata the offer and
// the transfer read.
type githubReleaseResponse struct {
	Assets []consoleReleaseAsset `json:"assets"`
}

// resolveConsoleDownloadOffer resolves the metadata the notice needs to
// offer the download of the paired console program: the exact asset of this
// release's own tag and an authoritative SHA-256 source for that same asset.
//
// It returns no offer — the notice keeps its manual instructions only — when
// the desktop name carries no release version, when the release does not
// publish exactly the paired asset, when no authoritative checksum source
// exists, or when the release cannot be read at all: a download is only ever
// offered where it can be verified, and a machine without a network gets the
// manual path instead of a button that would fail.
func resolveConsoleDownloadOffer(desktop string) (*consoleDownloadOffer, error) {
	release, ok := consoleReleaseFor(desktop)
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), consoleReleaseTimeout)
	defer cancel()

	metadata, err := fetchConsoleRelease(ctx, release.Tag)
	if err != nil {
		return nil, err
	}
	var asset *consoleReleaseAsset
	checksumsURL := ""
	for index := range metadata.Assets {
		assetMeta := &metadata.Assets[index]
		switch {
		// The asset matches the paired name case-insensitively: the
		// local lookup that found the pair compares names the way the
		// Windows file system does, so the release metadata must not
		// narrow that rule and silently drop the offer for a
		// case-renamed program.
		case strings.EqualFold(assetMeta.Name, release.Asset):
			asset = assetMeta
		case assetMeta.Name == releaseChecksumsAssetName && allowedReleaseDownloadURL(assetMeta.BrowserDownloadURL):
			checksumsURL = assetMeta.BrowserDownloadURL
		}
	}
	if asset == nil {
		return nil, fmt.Errorf("the release %s does not publish %s", release.Tag, release.Asset)
	}
	if asset.Size <= 0 {
		return nil, fmt.Errorf("the release %s publishes %s with an empty size", release.Tag, release.Asset)
	}
	if !allowedReleaseDownloadURL(asset.BrowserDownloadURL) {
		return nil, fmt.Errorf("the release %s does not publish %s from the official release downloads", release.Tag, release.Asset)
	}
	// Fail closed before the button exists: without an authoritative
	// checksum of this release there is nothing the download could be
	// verified against, so no download is offered.
	if !validSHA256Digest(asset.Digest) && checksumsURL == "" {
		return nil, fmt.Errorf("the release %s publishes no SHA-256 checksum for %s", release.Tag, release.Asset)
	}
	return &consoleDownloadOffer{
		Release:      release,
		Size:         asset.Size,
		URL:          asset.BrowserDownloadURL,
		Digest:       asset.Digest,
		ChecksumsURL: checksumsURL,
	}, nil
}

// fetchConsoleRelease reads the metadata of the release with this exact tag
// from the GitHub API of this repository.
func fetchConsoleRelease(ctx context.Context, tag string) (githubReleaseResponse, error) {
	endpoint := githubAPIBaseURL + "/repos/" + releaseRepository + "/releases/tags/" + url.PathEscape(tag)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubReleaseResponse{}, fmt.Errorf("the release %s cannot be read: %w", tag, err)
	}
	request.Header.Set("User-Agent", "hfdesk-desktop")
	request.Header.Set("Accept", "application/vnd.github+json")

	response, err := consoleHTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return githubReleaseResponse{}, ctx.Err()
		}
		return githubReleaseResponse{}, fmt.Errorf("the release %s cannot be read: %w", tag, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubReleaseResponse{}, fmt.Errorf("the release %s cannot be read: the release API answered HTTP %d", tag, response.StatusCode)
	}

	var release githubReleaseResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return githubReleaseResponse{}, fmt.Errorf("the release %s cannot be read: %w", tag, err)
	}
	return release, nil
}

// consoleDownloadError reports a failed transfer together with a partial
// file that cleanup could not remove — the one case the failure report must
// not claim that nothing was left behind.
type consoleDownloadError struct {
	cause    error
	leftover string
}

func (e *consoleDownloadError) Error() string {
	if e == nil || e.cause == nil {
		return "the download failed"
	}
	return e.cause.Error()
}

func (e *consoleDownloadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// errConsoleDownloadCanceled is the reason of a transfer the user cancelled.
// The progress dialog cancels the context of the transfer, so every path
// that observes that cancellation reports this sentence instead of a raw
// context error.
var errConsoleDownloadCanceled = errors.New("the download was cancelled")

// consoleDownloadTempPattern is the staging name of the transfer inside the
// folder of the desktop app. It never starts with "hfdesk" and never ends in
// ".exe", so a staged payload is never picked up as a console program
// candidate while it exists.
const consoleDownloadTempPattern = ".hfdesk-console-*.tmp"

// consoleDownloadOutcome is what a transfer that finished leaves behind:
// where the program now is, and whether this transfer is the one that put
// it there.
//
// Placed is the fact the settlement of a user cancel needs: only a file
// this transfer placed may be removed when the user cancels after the
// transfer already completed. A file that pre-existed, or that appeared
// while the bytes were in flight or between the destination check and the
// placement, was never this transfer's to delete and keeps the protection
// the placement gave it.
type consoleDownloadOutcome struct {
	// Path is the file the caller continues with when the outcome is
	// handed over; the staging file never carries this name.
	Path string
	// Placed reports whether this transfer placed its verified staging
	// file at Path, as opposed to finding the file already there.
	Placed bool
}

// consoleDestinationChecked runs after the destination check of the
// download and before the placement of the verified file — the window
// between the two, in which a file can still appear. It exists so a test
// can create that file and prove the placement keeps it; it is nil while
// the product runs.
var consoleDestinationChecked func(target string)

// downloadConsoleRelease fetches the exact asset of the offer into destDir,
// verifies it against the SHA-256 of that same release and publishes it
// under its release name.
//
// Nothing else ever appears at the destination: the checksum is resolved
// first, so a release without an authoritative checksum never even creates a
// staging file; the payload is staged under a name no lookup can mistake for
// the program; cancel, network, over-length and verification failures remove
// the staging file; and the final name appears only through one placement
// after the digest matched — a hard link, which the file system only creates
// when the name is free, so a file that is already there is never replaced.
// The write itself is bounded by the size the release metadata publishes, so
// a hostile or broken body that keeps sending cannot fill the disk before
// the digest check rejects it. progress, when set, reports the bytes read
// against the size the release metadata states.
//
// The outcome's Placed fact distinguishes the placement above from the case
// where the target already existed — or appeared in the window after the
// destination check — which the outcome keeps untouched.
func downloadConsoleRelease(ctx context.Context, offer *consoleDownloadOffer, destDir string, progress func(downloaded, total int64)) (outcome consoleDownloadOutcome, err error) {
	if offer == nil {
		return consoleDownloadOutcome{}, errors.New("no download of the console program was offered")
	}
	// The transfer only ever reads from the official release downloads of
	// this repository, whatever the offer was built from — an answer that
	// points elsewhere is refused before a single byte is requested.
	if !allowedReleaseDownloadURL(offer.URL) {
		return consoleDownloadOutcome{}, fmt.Errorf("the download of %s is not from the official release downloads of %s",
			offer.Release.Asset, releaseRepository)
	}
	// The advertised size is the bound of the write, so an offer that
	// publishes no size cannot be bounded and fails before any file
	// exists — the same fail-closed rule the checksum source follows.
	if offer.Size <= 0 {
		return consoleDownloadOutcome{}, fmt.Errorf("release %s publishes no size for %s, so the download cannot be bounded",
			offer.Release.Tag, offer.Release.Asset)
	}
	expected, err := releaseSHA256(ctx, offer)
	if err != nil {
		return consoleDownloadOutcome{}, err
	}

	temp, err := os.CreateTemp(destDir, consoleDownloadTempPattern)
	if err != nil {
		return consoleDownloadOutcome{}, fmt.Errorf("the folder %s cannot be written: %w", destDir, err)
	}
	tempPath := temp.Name()
	// Every path removes its staging file here — the placement publishes
	// the verified payload under the destination name and the staging
	// name is dropped beside it, and a path that placed nothing drops its
	// partial — and a removal that fails is reported instead of hidden:
	// a cancelled or failed download must leave nothing behind, and if it
	// cannot, the user has to be told which file to delete.
	defer func() {
		temp.Close()
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = &consoleDownloadError{
				cause:    errors.Join(err, fmt.Errorf("the staging file %s could not be removed: %w", tempPath, removeErr)),
				leftover: tempPath,
			}
		}
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, offer.URL, nil)
	if err != nil {
		return consoleDownloadOutcome{}, fmt.Errorf("the download of %s cannot be started: %w", offer.Release.Asset, err)
	}
	request.Header.Set("User-Agent", "hfdesk-desktop")
	response, err := consoleHTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return consoleDownloadOutcome{}, errConsoleDownloadCanceled
		}
		return consoleDownloadOutcome{}, fmt.Errorf("the download did not finish: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return consoleDownloadOutcome{}, fmt.Errorf("the download did not finish: the release download answered HTTP %d", response.StatusCode)
	}

	digest := sha256.New()
	// One byte beyond the bound is read so a body that stops at the bound
	// and one that runs past it are told apart: the byte is staged and
	// hashed like every other — the staging file never reaches the
	// destination in either case — and a body that runs past the bound
	// fails the download below, before the digest is even compared, so
	// over-length data can never be published.
	reader := &consoleProgressReader{
		source: io.LimitReader(response.Body, offer.Size+1),
		total:  offer.Size,
		report: progress,
	}
	written, err := io.Copy(io.MultiWriter(temp, digest), reader)
	if err != nil {
		if ctx.Err() != nil {
			return consoleDownloadOutcome{}, errConsoleDownloadCanceled
		}
		return consoleDownloadOutcome{}, fmt.Errorf("the download did not finish: %w", err)
	}
	if written > offer.Size {
		return consoleDownloadOutcome{}, fmt.Errorf("the download did not finish: the release download sent more than the %s it publishes for %s",
			consoleSizeText(offer.Size), offer.Release.Asset)
	}
	if err := temp.Close(); err != nil {
		return consoleDownloadOutcome{}, fmt.Errorf("the downloaded program could not be written to disk: %w", err)
	}

	got := hex.EncodeToString(digest.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return consoleDownloadOutcome{}, fmt.Errorf("the SHA-256 of the downloaded file is %s, but release %s publishes %s for it",
			got, offer.Release.Tag, expected)
	}

	target := filepath.Join(destDir, offer.Release.Asset)
	if _, statErr := os.Stat(target); statErr == nil {
		// The file was already there when the transfer finished — it
		// pre-existed or appeared while the bytes were in flight. The
		// action continues with the file that is there — the state the
		// flow had before this offer existed — and the verified copy is
		// discarded by the cleanup instead of replacing a file this
		// download never saw. Placed stays false: the file is not
		// this transfer's to remove on a later cancel.
		return consoleDownloadOutcome{Path: target}, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return consoleDownloadOutcome{}, fmt.Errorf("the folder %s cannot be checked: %w", destDir, statErr)
	}
	// The window the check above cannot see ends here: a file can appear
	// between the check and the placement — the pair the user handplaces
	// while the verified bytes wait — and the placement has to keep it.
	// The seam is where the test creates that file; the product leaves it
	// nil.
	if consoleDestinationChecked != nil {
		consoleDestinationChecked(target)
	}
	// The destination name is taken with a hard link, which the file
	// system only creates when the name is free: a file that took the
	// name after the check survives the placement untouched, becomes the
	// file the action continues with and keeps Placed false, so a later
	// cancel never removes it either. A link that fails while no file
	// stands at the name reports the failure instead — this transfer
	// never replaces a file it did not place, not even in that window.
	if err := os.Link(tempPath, target); err != nil {
		if _, statErr := os.Lstat(target); statErr == nil {
			return consoleDownloadOutcome{Path: target}, nil
		}
		return consoleDownloadOutcome{}, fmt.Errorf("the verified program could not be placed in %s: %w", destDir, err)
	}
	return consoleDownloadOutcome{Path: target, Placed: true}, nil
}

// consoleDialogClose tracks how the progress dialog of the transfer ends.
//
// The dialog has one button — Cancel — so the same id ends it when the user
// presses it (or presses Esc or the close button) and when the dialog ends
// it itself after the transfer produced its result. The tracker keeps the
// two apart: the programmatic close announces itself through transferSettled
// before it clicks, so a dialog that ends without that announcement was
// ended by the user. The dialog callback and the code that reads the tracker
// run on the one thread that owns the dialog, so no lock is needed.
type consoleDialogClose struct {
	programmatic bool
}

// transferSettled announces that the transfer produced its result, so the
// close the dialog performs next reads as the programmatic close it is and
// never as a cancel of the user.
func (c *consoleDialogClose) transferSettled() {
	c.programmatic = true
}

// userCancelled reports whether the user ended the dialog: a dialog that
// closed without the programmatic close was closed by the user's Cancel, Esc
// or close button, while a dialog whose run failed was answered by no one —
// that failure has its own report and never counts as a cancel.
func (c *consoleDialogClose) userCancelled(dialogErr error) bool {
	return dialogErr == nil && !c.programmatic
}

// settleConsoleDownload decides what the ended progress dialog of the
// transfer leaves behind for its caller.
//
// The three ways the dialog ends have three different outcomes. The user's
// cancel is the user's decision even in the window after the transfer
// already finished and verified: the file this transfer placed is removed,
// a file the transfer did not place keeps its protection, and the
// cancellation is what the caller reports — the service action does not
// continue. The dialog's own programmatic close is not a cancel: a transfer
// that finished and verified continues the action with its file. A dialog
// failure decides nothing about the user — it continues with a file the
// transfer verified, reports the dialog when that failure cancelled the
// transfer, and otherwise yields to the transfer's own error.
func settleConsoleDownload(userCancelled bool, dialogErr error, outcome consoleDownloadOutcome, transferErr error) (string, error) {
	switch {
	case userCancelled:
		if transferErr != nil {
			// The cancel reached the transfer while it ran: its
			// own error carries the cause, and no file was ever
			// placed.
			return "", transferErr
		}
		if outcome.Placed {
			if removeErr := os.Remove(outcome.Path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return "", &consoleDownloadError{
					cause: errors.Join(errConsoleDownloadCanceled,
						fmt.Errorf("the cancelled download left %s behind and it could not be removed: %w", outcome.Path, removeErr)),
					leftover: outcome.Path,
				}
			}
		}
		return "", errConsoleDownloadCanceled
	case dialogErr != nil:
		if transferErr == nil {
			// The transfer finished and verified before the
			// dialog failed; the file is usable, so the action
			// continues with it.
			return outcome.Path, nil
		}
		if errors.Is(transferErr, errConsoleDownloadCanceled) {
			// The dialog failed right away and its failure is
			// what cancelled the transfer: the report names that
			// cause instead of a cancellation the user never
			// asked for.
			return "", fmt.Errorf("the progress dialog could not be shown, so nothing was downloaded: %w", dialogErr)
		}
		return "", transferErr
	case transferErr != nil:
		return "", transferErr
	default:
		return outcome.Path, nil
	}
}

// releaseSHA256 returns the authoritative SHA-256 of the offered asset: the
// digest the release API publishes for it, or — when the API carries none —
// its line in the release's checksums.txt. A release that offers neither
// source fails closed here, before any file exists.
func releaseSHA256(ctx context.Context, offer *consoleDownloadOffer) (string, error) {
	if validSHA256Digest(offer.Digest) {
		return strings.TrimPrefix(strings.ToLower(offer.Digest), "sha256:"), nil
	}
	if offer.ChecksumsURL == "" || !allowedReleaseDownloadURL(offer.ChecksumsURL) {
		return "", fmt.Errorf("the release %s publishes no SHA-256 checksum for %s",
			offer.Release.Tag, offer.Release.Asset)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, offer.ChecksumsURL, nil)
	if err != nil {
		return "", fmt.Errorf("the checksum list of release %s cannot be read: %w", offer.Release.Tag, err)
	}
	request.Header.Set("User-Agent", "hfdesk-desktop")
	response, err := consoleHTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", errConsoleDownloadCanceled
		}
		return "", fmt.Errorf("the checksum list of release %s cannot be read: %w", offer.Release.Tag, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the checksum list of release %s cannot be read: the release download answered HTTP %d",
			offer.Release.Tag, response.StatusCode)
	}
	// The list covers one line per asset of the release, so a megabyte is
	// far beyond its real size and a bound keeps a broken answer from
	// being read whole.
	list, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return "", errConsoleDownloadCanceled
		}
		return "", fmt.Errorf("the checksum list of release %s cannot be read: %w", offer.Release.Tag, err)
	}
	digest, ok := sha256FromChecksums(string(list), offer.Release.Asset)
	if !ok {
		return "", fmt.Errorf("the release %s publishes no SHA-256 checksum for %s",
			offer.Release.Tag, offer.Release.Asset)
	}
	return digest, nil
}

// sha256FromChecksums finds the SHA-256 a sha256sum checksums list states
// for exactly this asset name. The list is authoritative for the release it
// was published with, and a line for another file is never taken as this
// file's checksum. The name comparison is case-insensitive, the same rule
// the local pair lookup follows, so a case-renamed desktop program still
// resolves the line of its pair.
func sha256FromChecksums(list, asset string) (string, bool) {
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) != 2 || !strings.EqualFold(strings.TrimPrefix(fields[1], "*"), asset) {
			continue
		}
		if !validSHA256Hex(fields[0]) {
			return "", false
		}
		return strings.ToLower(fields[0]), true
	}
	return "", false
}

// validSHA256Digest reports whether a release asset digest is a SHA-256 the
// verifier can use; any other algorithm or shape counts as no digest.
func validSHA256Digest(digest string) bool {
	hexPart, found := strings.CutPrefix(strings.ToLower(digest), "sha256:")
	return found && validSHA256Hex(hexPart)
}

// validSHA256Hex reports whether a string is exactly one SHA-256 in hex.
func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// allowedReleaseDownloadURL reports whether a URL is inside the official
// release downloads of this repository.
func allowedReleaseDownloadURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, releaseDownloadAllowedPrefix)
}

// consoleProgressReader reports the bytes read against the total the release
// metadata states, so the dialog shows the transfer's own progress.
type consoleProgressReader struct {
	source io.Reader
	read   int64
	total  int64
	report func(downloaded, total int64)
}

func (r *consoleProgressReader) Read(buffer []byte) (int, error) {
	read, err := r.source.Read(buffer)
	if read > 0 {
		r.read += int64(read)
		if r.report != nil {
			r.report(r.read, r.total)
		}
	}
	return read, err
}

// consoleSizeText renders a byte count for the note line of the notice, in
// the units a user reads a program size in.
func consoleSizeText(size int64) string {
	switch {
	case size < 0:
		return "an unknown size"
	case size < 1024:
		return fmt.Sprintf("%d bytes", size)
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}
