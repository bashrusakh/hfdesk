// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Opt-in normal-cache local-I/O benchmark:
// go test -run '^$' -bench BenchmarkRangedFinalization ./pkg/hfdownloader/
// BENCHSIZE sets synthetic payload bytes (default 1 GiB).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func benchFileSize() int64 {
	if v := os.Getenv("BENCHSIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 1 << 30
}

// benchPatternReader repeats block without materializing the payload. off
// preserves the pattern position across independently requested ranges.
type benchPatternReader struct {
	remain int64
	block  []byte
	off    int64
}

func (r *benchPatternReader) Read(p []byte) (int, error) {
	if r.remain <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remain {
		n = r.remain
	}
	blockLen := int64(len(r.block))
	for written := int64(0); written < n; {
		chunk := n - written
		if avail := blockLen - r.off; chunk > avail {
			chunk = avail
		}
		copy(p[written:written+chunk], r.block[r.off:r.off+chunk])
		written += chunk
		r.off = (r.off + chunk) % blockLen
	}
	r.remain -= n
	return int(n), nil
}

type cacheDownloadMeasurement struct {
	completionToDone time.Duration
	postReadBytes    int64
	postWriteBytes   int64
	preWriteBytes    int64
}

func runCacheDownloadMeasurement(tb testing.TB, size int64, mode string) cacheDownloadMeasurement {
	tb.Helper()
	block := make([]byte, 4<<20)
	for i := range block {
		block[i] = byte(i % 251)
	}
	h := sha256.New()
	if _, err := io.Copy(h, &benchPatternReader{remain: size, block: block}); err != nil {
		tb.Fatalf("hash synthetic setup payload: %v", err)
	}
	sha := hex.EncodeToString(h.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"bench-commit"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte("[" + lfsTreeNode("bench.bin", int(size), sha) + "]"))
		case strings.Contains(r.URL.Path, "/resolve/"):
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= size {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			reader := &benchPatternReader{remain: end - start + 1, block: block, off: start % int64(len(block))}
			if _, err := io.Copy(w, reader); err != nil {
				return
			}
		default:
			http.NotFound(w, r)
		}
	}))
	tb.Cleanup(srv.Close)

	cacheDir := tb.TempDir()
	measurement := &payloadIOMeasurement{}
	ctx := withPayloadIOMeasurement(context.Background(), measurement)
	var networkComplete, fileDone time.Time
	var readsAtComplete, writesAtComplete int64
	err := Download(ctx, Job{Repo: "owner/model", Revision: "main"}, Settings{
		CacheDir:           cacheDir,
		Endpoint:           srv.URL,
		Concurrency:        4,
		MaxActiveDownloads: 1,
		MultipartThreshold: "1KB",
		Verify:             mode,
		Retries:            0,
		StallTimeout:       "0",
	}, func(ev ProgressEvent) {
		if ev.Event == "file_finalizing" && networkComplete.IsZero() {
			// The ranged path emits this synchronously after every response body
			// reached clean EOF and before proof/publication starts.
			networkComplete = time.Now()
			readsAtComplete, writesAtComplete = measurement.snapshot()
		}
		if ev.Event == "file_done" {
			fileDone = time.Now()
		}
	})
	if err != nil {
		tb.Fatalf("Download: %v", err)
	}
	if networkComplete.IsZero() || fileDone.IsZero() || fileDone.Before(networkComplete) {
		tb.Fatalf("missing or out-of-order client completion events: network=%v file_done=%v", networkComplete, fileDone)
	}
	readsAtDone, writesAtDone := measurement.snapshot()
	if writesAtComplete != size {
		tb.Fatalf("local payload writes at network completion=%d, want %d transferred bytes", writesAtComplete, size)
	}
	repo, err := NewHFCache(cacheDir).Repo("owner/model", RepoTypeModel)
	if err != nil {
		tb.Fatalf("cache repo: %v", err)
	}
	snapshot, err := repo.SnapshotPath("bench-commit", "bench.bin")
	if err != nil {
		tb.Fatalf("snapshot path: %v", err)
	}
	paths := []string{repo.BlobPath(sha), snapshot, filepath.Join(repo.FriendlyPath(), "bench.bin")}
	for _, path := range paths {
		fi, statErr := os.Stat(path)
		if statErr != nil {
			tb.Fatalf("required cache payload path %s is not reachable: %v", path, statErr)
		}
		if fi.Size() != size {
			tb.Fatalf("cache path %s payload size=%d, want %d", path, fi.Size(), size)
		}
	}
	return cacheDownloadMeasurement{
		completionToDone: fileDone.Sub(networkComplete),
		postReadBytes:    readsAtDone - readsAtComplete,
		postWriteBytes:   writesAtDone - writesAtComplete,
		preWriteBytes:    writesAtComplete,
	}
}

func TestCacheDownloadMeasurementBoundary(t *testing.T) {
	const size = 64 * 1024
	sizeMode := runCacheDownloadMeasurement(t, size, "size")
	shaMode := runCacheDownloadMeasurement(t, size, "sha256")
	if sizeMode.completionToDone < 0 || shaMode.completionToDone < 0 {
		t.Fatal("network-complete to file_done duration must be non-negative")
	}
	if sizeMode.postReadBytes != 0 || sizeMode.postWriteBytes != 0 {
		t.Errorf("Verify=size post-network payload I/O = read %d, write %d; want none on normal same-filesystem cache publication", sizeMode.postReadBytes, sizeMode.postWriteBytes)
	}
	if shaMode.postReadBytes != size || shaMode.postWriteBytes != 0 {
		t.Errorf("Verify=sha256 post-network payload I/O = read %d, write %d; want one %d-byte proof read and no rewrite", shaMode.postReadBytes, shaMode.postWriteBytes, size)
	}
}

func TestPayloadIOMeasurementCountsCacheCopyFallback(t *testing.T) {
	content := bytes.Repeat([]byte("fallback-payload"), 128)
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "destination")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	measurement := &payloadIOMeasurement{}
	if err := copyFileAtomicCtx(withPayloadIOMeasurement(context.Background(), measurement), src, dst); err != nil {
		t.Fatalf("copy fallback: %v", err)
	}
	reads, writes := measurement.snapshot()
	if reads != int64(len(content)) || writes != int64(len(content)) {
		t.Errorf("copy fallback I/O = read %d, write %d; want %d bytes each", reads, writes, len(content))
	}
	assertExactFile(t, dst, content)
}

// BenchmarkRangedFinalization measures the public HF-cache Download boundary
// from client-observed network completion to file_done, including payload I/O
// in proof and any cache copy fallback. Snapshot/friendly entries are checked
// reachable and refer to the published blob in both verification modes.
func BenchmarkRangedFinalization(b *testing.B) {
	size := benchFileSize()
	for _, mode := range []string{"size", "sha256"} {
		b.Run("verify="+mode, func(b *testing.B) {
			b.SetBytes(size)
			for i := 0; i < b.N; i++ {
				m := runCacheDownloadMeasurement(b, size, mode)
				b.ReportMetric(float64(m.preWriteBytes)/float64(size), "preCompleteWrites×S")
				b.ReportMetric(float64(m.postReadBytes)/float64(size), "postCompleteReads×S")
				b.ReportMetric(float64(m.postWriteBytes)/float64(size), "postCompleteWrites×S")
				b.ReportMetric(float64(m.completionToDone.Microseconds()), "networkCompleteToFileDoneUs")
			}
		})
	}
}
