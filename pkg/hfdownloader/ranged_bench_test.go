// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Opt-in local-I/O finalization benchmark:
// go test -run '^$' -bench BenchmarkRangedFinalization ./pkg/hfdownloader/
// BENCHSIZE sets payload bytes (default 1 GiB).

import (
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
	"testing"
	"time"
)

// Small indirection so the benchmark's setup hashing does not go through
// computeSHA256Ctx (which would pollute the hash-pass counters it reports).
type benchHash struct {
	h interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
}

func newBenchHash() *benchHash               { return &benchHash{h: sha256.New()} }
func benchHashWrite(bh *benchHash, p []byte) { _, _ = bh.h.Write(p) }
func benchHashSum(bh *benchHash) string      { return hex.EncodeToString(bh.h.Sum(nil)) }

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

// BenchmarkRangedFinalization compares size and sha256 completion on loopback.
// Per-iteration metrics (S is payload size):
//   - stagingWriteBytes×S: all staging payload writes divided by S, including transfer.
//   - extraLocalWrites×S: max(staging writes - S, 0) / S, not total local I/O.
//   - hashReadBytes×S: counted full-file SHA reads / S (size: 0, sha256: 1 here).
//   - finalizeMs: file_finalizing to successful publication, including optional
//     proof and publication cleanup, excluding caller cache links and manifest.
//   - totalUs: timed domain setup and downloadMultipart, including loopback transfer
//     and finalization; not the top-level Download job.
func BenchmarkRangedFinalization(b *testing.B) {
	size := benchFileSize()

	block := make([]byte, 4<<20)
	for i := range block {
		block[i] = byte(i % 251)
	}

	newServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || end >= size {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			pr := &benchPatternReader{remain: end - start + 1, block: block, off: start % int64(len(block))}
			buf := make([]byte, 512<<10)
			for {
				n, err := pr.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}))
	}

	for _, mode := range []string{"size", "sha256"} {
		b.Run("verify="+mode, func(b *testing.B) {
			b.SetBytes(size)
			it := PlanItem{RelativePath: "bench.bin", Size: size, AcceptRanges: true}
			if mode == "sha256" {
				h := newBenchHash()
				pr := &benchPatternReader{remain: size, block: block}
				buf := make([]byte, 512<<10)
				for {
					n, err := pr.Read(buf)
					if n > 0 {
						benchHashWrite(h, buf[:n])
					}
					if err != nil {
						break
					}
				}
				it.LFS = true
				it.SHA256 = benchHashSum(h)
			}
			for i := 0; i < b.N; i++ {
				tmpDir := b.TempDir()
				dst := filepath.Join(tmpDir, "bench.bin")
				srv := newServer()
				it.URL = srv.URL + "/bench.bin"
				cfg := Settings{Concurrency: 4, Retries: 0, StallTimeout: "0", Verify: mode}

				stagingWriteBytes.Store(0)
				hashPassCount.Store(0)
				hashPassBytes.Store(0)

				start := time.Now()
				if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, withTestDomain(b, dst, multipartOpts{}), func(ProgressEvent) {}); err != nil {
					b.Fatalf("downloadMultipart: %v", err)
				}
				elapsed := time.Since(start)
				srv.Close()

				written := stagingWriteBytes.Load()
				read := hashPassBytes.Load()
				extraWrites := written - size
				if extraWrites < 0 {
					extraWrites = 0
				}
				b.ReportMetric(float64(written)/float64(size), "stagingWriteBytes×S")
				b.ReportMetric(float64(extraWrites)/float64(size), "extraLocalWrites×S")
				b.ReportMetric(float64(read)/float64(size), "hashReadBytes×S")
				b.ReportMetric(float64(finalizationNanos.Load())/1e6, "finalizeMs")
				b.ReportMetric(float64(elapsed.Microseconds()), "totalUs")

				fi, err := os.Stat(dst)
				if err != nil || fi.Size() != size {
					b.Fatalf("published file wrong: %v", err)
				}
			}
		})
	}
}
