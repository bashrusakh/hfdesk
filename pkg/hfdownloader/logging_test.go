// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// logCapture is a concurrency-safe buffer that collects what a logger writes
// to it.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// installCapturingPipeline installs a logging pipeline whose console sink
// writes into an in-memory buffer and restores the previous default logger
// when the test ends.
//
// The pipeline is installed process wide because that is how the application
// installs it; the downloader package itself never installs one.
func installCapturingPipeline(t *testing.T) (*logging.Pipeline, *logCapture) {
	t.Helper()
	captured := &logCapture{}
	pipeline, err := logging.New(logging.Options{Console: true, ConsoleWriter: captured})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	previous := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(pipeline.Logger())
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return pipeline, captured
}

// installRawLogger installs a plain slog handler that writes into an
// in-memory buffer and restores the previous default logger when the test
// ends.
//
// The handler performs no redaction, so a credential that reaches a record
// shows up verbatim in the buffer. It is used to prove that the downloader
// itself never puts a credential into a record, independently of the
// pipeline's redaction.
func installRawLogger(t *testing.T) *logCapture {
	t.Helper()
	captured := &logCapture{}
	previous := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(captured, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return captured
}

// newTestHub starts an offline stand-in for the Hugging Face Hub. It serves
// the revision, the tree and the content of a one-file repository, and it
// records the Authorization header of every request it received.
func newTestHub(t *testing.T, content []byte) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu       sync.Mutex
		authSeen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/o/r/revision/main":
			json.NewEncoder(w).Encode(RepoInfo{SHA: "abc123"})
		case "/api/models/o/r/tree/abc123":
			json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: int64(len(content))}})
		case "/o/r/raw/abc123/config.json":
			mu.Lock()
			authSeen = append(authSeen, r.Header.Get("Authorization"))
			mu.Unlock()
			w.Write(content)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), authSeen...)
	}
}

// TestCleanupWarningReachesPipelineWithDownloadComponent proves that a
// migrated cleanup warning reaches the application pipeline with the
// download component and at warning level, and that the downloader no longer
// writes that message through the standard library logger.
//
// A .part path that is a non-empty directory cannot be removed, so the
// cleanup reaches the branch that reports a failure it cannot act on.
func TestCleanupWarningReachesPipelineWithDownloadComponent(t *testing.T) {
	pipeline, captured := installCapturingPipeline(t)

	outputDir := t.TempDir()
	dst := filepath.Join(outputDir, "owner", "repo", "model.bin")
	if err := os.MkdirAll(dst+".part", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst+".part", "keep"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CleanupJobPartFiles(Settings{OutputDir: outputDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	records, _ := pipeline.Records(0, logging.OldestFirst)
	var found *logging.Record
	for i := range records {
		if records[i].Message == "cleanup partial file failed" {
			found = &records[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no cleanup warning reached the pipeline; records: %+v", records)
	}
	if found.Component != "download" {
		t.Errorf("component = %q, want %q", found.Component, "download")
	}
	if found.Level != slog.LevelWarn {
		t.Errorf("level = %v, want %v", found.Level, slog.LevelWarn)
	}
	if got, ok := found.Attrs["path"].(string); !ok || got != dst+".part" {
		t.Errorf("path attribute = %v, want %q", found.Attrs["path"], dst+".part")
	}

	// Every record the downloader produced carries the download component. A
	// record without one would mean the standard library logger is still in
	// use: slog.SetDefault routes the standard logger into the pipeline.
	for _, rec := range records {
		if rec.Component != "download" {
			t.Errorf("record %q has component %q, want %q", rec.Message, rec.Component, "download")
		}
	}

	out := captured.String()
	if !strings.Contains(out, "[download] cleanup partial file failed") {
		t.Errorf("captured output %q does not contain the migrated cleanup warning", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Errorf("captured output %q does not carry the warning level", out)
	}
}

// TestDownloadLifecycleRecordsReachPipeline proves that the download
// lifecycle facts reach the application pipeline with the download component:
// the job start, the plan, the per-file start and completion, and the
// completion of the job.
func TestDownloadLifecycleRecordsReachPipeline(t *testing.T) {
	pipeline, captured := installCapturingPipeline(t)

	srv, _ := newTestHub(t, []byte("payload"))
	cfg := Settings{
		OutputDir: t.TempDir(),
		Endpoint:  srv.URL,
		Retries:   1,
	}
	if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
		t.Fatalf("Download: %v", err)
	}

	records, _ := pipeline.Records(0, logging.OldestFirst)
	wantMessages := []string{
		"download started",
		"download planned",
		"file download started",
		"file download complete",
		"download complete",
	}
	for _, want := range wantMessages {
		if !hasRecord(records, want, slog.LevelInfo) {
			t.Errorf("no info record %q reached the pipeline; records: %+v", want, records)
		}
	}

	// The plan record carries the facts the planner already knew.
	for _, rec := range records {
		if rec.Message != "download planned" {
			continue
		}
		if got, ok := rec.Attrs["files"].(int64); !ok || got != 1 {
			t.Errorf("files attribute = %v, want 1", rec.Attrs["files"])
		}
		if got, ok := rec.Attrs["repo"].(string); !ok || got != "o/r" {
			t.Errorf("repo attribute = %v, want %q", rec.Attrs["repo"], "o/r")
		}
	}

	for _, rec := range records {
		if rec.Component != "download" {
			t.Errorf("record %q has component %q, want %q", rec.Message, rec.Component, "download")
		}
	}
	if !strings.Contains(captured.String(), "[download] download complete") {
		t.Errorf("captured output %q does not contain the completion record", captured.String())
	}
}

// TestDownloadFailureRecordsReachPipeline proves that a failing download is
// recorded: the file failure and the job failure both reach the pipeline with
// the download component and at error level.
func TestDownloadFailureRecordsReachPipeline(t *testing.T) {
	pipeline, _ := installCapturingPipeline(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/o/r/revision/main":
			json.NewEncoder(w).Encode(RepoInfo{SHA: "abc123"})
		case "/api/models/o/r/tree/abc123":
			json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: 7}})
		default:
			// A permanent status: the file fails and the job fails with it.
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	cfg := Settings{
		OutputDir: t.TempDir(),
		Endpoint:  srv.URL,
		Retries:   5,
	}
	err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("Download succeeded; expected the permanent status to fail the job")
	}

	records, _ := pipeline.Records(0, logging.OldestFirst)
	if !hasRecord(records, "file download failed", slog.LevelError) {
		t.Errorf("no error record %q reached the pipeline; records: %+v", "file download failed", records)
	}
	if !hasRecord(records, "download failed", slog.LevelError) {
		t.Errorf("no error record %q reached the pipeline; records: %+v", "download failed", records)
	}
	for _, rec := range records {
		if rec.Component != "download" {
			t.Errorf("record %q has component %q, want %q", rec.Message, rec.Component, "download")
		}
	}
}

// TestDownloadCancellationRecordReachesPipeline proves that a cancelled
// download is recorded as a cancellation and not as a failure: the job
// outcome is reported at info level.
func TestDownloadCancellationRecordReachesPipeline(t *testing.T) {
	pipeline, _ := installCapturingPipeline(t)

	// The file request blocks until the client cancels it, so the
	// cancellation is the only way the transfer can end.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/o/r/revision/main":
			json.NewEncoder(w).Encode(RepoInfo{SHA: "abc123"})
		case "/api/models/o/r/tree/abc123":
			json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: 7}})
		default:
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	cfg := Settings{
		OutputDir: t.TempDir(),
		Endpoint:  srv.URL,
		Retries:   1,
	}
	err := Download(ctx, Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {})
	if err == nil {
		t.Fatal("Download succeeded; expected the cancellation to fail the job")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want a context cancellation", err)
	}

	records, _ := pipeline.Records(0, logging.OldestFirst)
	if !hasRecord(records, "download cancelled", slog.LevelInfo) {
		t.Errorf("no info record %q reached the pipeline; records: %+v", "download cancelled", records)
	}
	if hasRecord(records, "download failed", slog.LevelError) {
		t.Errorf("a cancelled download was recorded as a failure; records: %+v", records)
	}
}

// TestDownloaderRecordsWithoutInstalledPipeline proves that the downloader
// still records when the application installed no logging pipeline: the
// standard library default logger is used then, and the record is not lost.
func TestDownloaderRecordsWithoutInstalledPipeline(t *testing.T) {
	// The standard library logger is where the default slog logger writes
	// when no pipeline is installed.
	var standard bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&standard)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	outputDir := t.TempDir()
	dst := filepath.Join(outputDir, "owner", "repo", "model.bin")
	if err := os.MkdirAll(dst+".part", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst+".part", "keep"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanupJobPartFiles(Settings{OutputDir: outputDir}, []string{dst}); err != nil {
		t.Fatalf("CleanupJobPartFiles: %v", err)
	}

	if !strings.Contains(standard.String(), "cleanup partial file failed") {
		t.Errorf("standard logger output %q does not contain the cleanup warning", standard.String())
	}
}

// TestDownloadLogsNeverCarryCredentials proves that the download records
// cannot carry a credential: the Hugging Face token, the Authorization
// header value and the proxy credentials never reach a record.
//
// The download really sends the token to the server and really runs with a
// misconfigured proxy, so the absence of the credentials from the records is
// a property of the records and not of an unused token or an unused proxy
// configuration.
func TestDownloadLogsNeverCarryCredentials(t *testing.T) {
	const (
		// The token shape the pipeline's redaction rules match.
		redactableToken = "hf_ThisIsTheSecretToken0123456789"
		// A token no redaction rule matches, so the raw handler below
		// shows it verbatim if the downloader ever emits it.
		plainToken = "hf_tok3"
		proxyUser  = "proxyuser"
		proxyPass  = "proxysecret"
	)

	cases := []struct {
		name  string
		token string
	}{
		{name: "token the pipeline redacts", token: redactableToken},
		{name: "token no redaction rule matches", token: plainToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secrets := []string{tc.token, "Bearer " + tc.token, proxyUser, proxyPass}

			// The raw handler performs no redaction: it proves that the
			// downloader itself never puts a credential into a record.
			raw := installRawLogger(t)
			srv, authSeen := newTestHub(t, []byte("payload"))
			cfg := Settings{
				OutputDir: t.TempDir(),
				Endpoint:  srv.URL,
				Token:     tc.token,
				Retries:   1,
				// A proxy URL that cannot be parsed and that carries
				// credentials: the downloader falls back to a direct
				// connection and records the reason.
				Proxy: &ProxyConfig{URL: fmt.Sprintf("http://%s:%s@proxy.example:notaport", proxyUser, proxyPass)},
			}
			if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg, func(ProgressEvent) {}); err != nil {
				t.Fatalf("Download: %v", err)
			}

			// The token really was sent, so its absence from the records
			// means the records never carried it.
			seen := authSeen()
			if len(seen) == 0 {
				t.Fatal("the server received no Authorization header")
			}
			for _, value := range seen {
				if value != "Bearer "+tc.token {
					t.Errorf("Authorization header = %q, want %q", value, "Bearer "+tc.token)
				}
			}

			rawOut := raw.String()
			for _, secret := range secrets {
				if strings.Contains(rawOut, secret) {
					t.Errorf("unredacted records carry the credential %q:\n%s", secret, rawOut)
				}
			}
			// The misconfigured proxy was really reported, so the proxy
			// path is covered and not silently skipped.
			if !strings.Contains(rawOut, "invalid proxy configuration") {
				t.Errorf("unredacted records do not report the proxy failure:\n%s", rawOut)
			}

			// The same guarantee on the application pipeline.
			pipeline, captured := installCapturingPipeline(t)
			srv2, _ := newTestHub(t, []byte("payload"))
			cfg2 := Settings{
				OutputDir: t.TempDir(),
				Endpoint:  srv2.URL,
				Token:     tc.token,
				Retries:   1,
				Proxy:     &ProxyConfig{URL: fmt.Sprintf("http://%s:%s@proxy.example:notaport", proxyUser, proxyPass)},
			}
			if err := Download(context.Background(), Job{Repo: "o/r", Revision: "main"}, cfg2, func(ProgressEvent) {}); err != nil {
				t.Fatalf("Download: %v", err)
			}

			out := captured.String()
			for _, secret := range secrets {
				if strings.Contains(out, secret) {
					t.Errorf("captured records carry the credential %q:\n%s", secret, out)
				}
			}
			records, _ := pipeline.Records(0, logging.OldestFirst)
			for _, rec := range records {
				for _, secret := range secrets {
					if strings.Contains(rec.Message, secret) {
						t.Errorf("record %q carries the credential %q", rec.Message, secret)
					}
					for key, value := range rec.Attrs {
						if text, ok := value.(string); ok && strings.Contains(text, secret) {
							t.Errorf("record %q attribute %q carries the credential %q", rec.Message, key, secret)
						}
					}
				}
			}
		})
	}
}

// TestProxyURLErrorCarriesNoCredentials proves that a proxy configuration
// failure cannot carry the proxy credentials: neither into an error that
// reaches the callers (and from there the API responses and the log
// records) nor into the human readable proxy description.
func TestProxyURLErrorCarriesNoCredentials(t *testing.T) {
	const (
		proxyUser = "proxyuser"
		proxyPass = "proxysecret"
	)

	cases := []struct {
		name string
		url  string
	}{
		{
			name: "invalid port",
			url:  fmt.Sprintf("http://%s:%s@proxy.example:notaport", proxyUser, proxyPass),
		},
		{
			name: "invalid host",
			url:  fmt.Sprintf("http://%s:%s@[::1", proxyUser, proxyPass),
		},
		{
			name: "invalid escape in the credential",
			url:  fmt.Sprintf("http://%s:pa%%zz%s@proxy.example", proxyUser, proxyPass),
		},
		{
			name: "empty scheme",
			url:  fmt.Sprintf("://%s:%s@proxy.example", proxyUser, proxyPass),
		},
		{
			// Without a scheme the credentials cannot be located, so the
			// failure must not quote any part of the URL.
			name: "no scheme",
			url:  fmt.Sprintf("1%s:pa%%zz%s@proxy.example", proxyUser, proxyPass),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildHTTPClient(&ProxyConfig{URL: tc.url})
			if err == nil {
				t.Fatal("expected the proxy URL to fail parsing")
			}
			message := err.Error()
			for _, secret := range []string{proxyUser, proxyPass, "%zz"} {
				if strings.Contains(message, secret) {
					t.Errorf("proxy error %q still carries %q", message, secret)
				}
			}
			if !strings.Contains(message, "invalid proxy URL") {
				t.Errorf("proxy error %q lost its context", message)
			}

			// The human readable description masks the password as well,
			// including when the URL does not parse at all.
			description := GetProxyInfoString(&ProxyConfig{URL: tc.url})
			for _, secret := range []string{proxyUser, proxyPass, "%zz"} {
				if strings.Contains(description, secret) {
					t.Errorf("proxy description %q still carries %q", description, secret)
				}
			}
		})
	}
}

// hasRecord reports whether the records contain a record with the given
// message and level.
func hasRecord(records []logging.Record, message string, level slog.Level) bool {
	for _, rec := range records {
		if rec.Message == message && rec.Level == level {
			return true
		}
	}
	return false
}
