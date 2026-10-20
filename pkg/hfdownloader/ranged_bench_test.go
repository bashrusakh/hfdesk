// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Opt-in local-I/O benchmark for the issue #140 finalization path (run with
// `go test -bench BenchmarkRangedFinalization ./pkg/hfdownloader/`). It is
// deliberately NOT part of the ordinary CI test run: it streams a large
// synthetic payload through a local httptest server and records the
// network-complete → file-done wall time plus the staging bytes written
// (including the transfer landing on disk) and full-file hash bytes read,
// via the stagingWriteBytes / hashPassBytes observability counters.
//
// Expected shapes (S = file size):
//
//   - pre-#140 path: ≈3×S post-network local I/O (assembly copy writes S,
//     the unconditional SHA reread reads S, on top of the transfer);
//   - new path, Verify=size: stagingWriteBytes is ≈1×S (the transfer itself),
//     extraLocalWrites is ≈0×S (no assembly copy), hashReadBytes is 0;
//   - new path, Verify=sha256: ≈1×S hash read, no assembly write.
//
// The synthetic file size defaults to 1 GiB; set BENCHSIZE (bytes) to scale
// it down on slow disks — the ratios are linear in S.

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

// benchFileSize is the synthetic payload size: 1 GiB unless overridden via
// the BENCHSIZE environment variable (bytes).
func benchFileSize() int64 {
	if v := os.Getenv("BENCHSIZE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 1 << 30
}

// benchPatternReader serves a deterministic repeating pattern without
// materializing the whole payload in memory. off is the pattern offset of the
// first byte, so a ranged response starts at the correct position within the
// repeated block instead of restarting the block at every range.
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

// BenchmarkRangedFinalization measures the local finalization cost after
// the network transfer completes, for the two verification modes that
// matter. Reported metrics per iteration:
//
//   - stagingWriteBytes×S: total payload bytes written into the staging file,
//     including the transfer itself landing on disk (a fresh transfer is
//     ≈1×S, so this metric alone is not "extra" local I/O);
//   - extraLocalWrites×S: stagingWriteBytes minus one file size, i.e. the
//     extra local payload writes after the network transfer (assembly-style
//     copies would show up here; the rename-only path is ≈0×S);
//   - hashReadBytes×S: full-file SHA-256 bytes read (Verify=size ≈0×S,
//     Verify=sha256 ≈1×S);
//   - finalizeMs: network-complete → file-published wall time: from the last
//     transferred byte (the file_finalizing signal) through the optional
//     strict hash to the publish rename, stopping at that rename. It excludes
//     caller-level work after downloadMultipart returns (cache snapshot/friendly
//     link creation, the manifest, and other job finalization). This is the
//     issue #140 finalization metric; it is reported alongside the byte counters
//     so a slow local step is attributable rather than hidden in totalUs;
//   - totalUs: the whole download's wall time (network + finalization on a
//     loopback server, so the local I/O share dominates for large S).
func BenchmarkRangedFinalization(b *testing.B) {
	size := benchFileSize()

	// 4 MiB pattern block; the payload is its repetition.
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
			// For the strict mode the item must be an LFS blob with its
			// expected digest; compute the pattern's real SHA-256 once so
			// the single post-range pass succeeds.
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
				if _, err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, multipartOpts{}, func(ProgressEvent) {}); err != nil {
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
