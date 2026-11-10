// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// strictDownloadResponse covers both admission shapes: a new job (202) puts
// the snapshot fields at the top level, a deduplicated request (200) nests
// the existing job under "job".
type strictDownloadResponse struct {
	ID      string `json:"id"`
	Job     *Job   `json:"job"`
	Message string `json:"message"`
	// error_detail carries the machine-readable code and the unescaped
	// details string; collision/naming assertions read it instead of the
	// raw body, whose quotes are JSON-escaped.
	ErrorDetail *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details string `json:"details"`
	} `json:"error_detail"`
}

func postStrict(mux *http.ServeMux, path string, req DownloadRequest) (int, strictDownloadResponse, string) {
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	var resp strictDownloadResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp, w.Body.String()
}

func findJobByID(jobs []*Job, id string) *Job {
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// TestStrictSelectionContract covers the strict file-selection request
// contract end to end: request-shape 400s before any Hub contact, plan
// parity, dedup identity across normalized spellings, the /api/jobs
// snapshot, jobs_state.json round-trip, and a real download that completes
// only if the plan contained exactly the selected files.
func TestStrictSelectionContract(t *testing.T) {
	release := make(chan struct{})
	var hubRequests atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubRequests.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_, _ = w.Write([]byte(`{"sha":"deadbeef"}`))
		case strings.Contains(r.URL.Path, "/tree/"):
			_, _ = w.Write([]byte(`[{"type":"file","path":"A.bin","size":4},{"type":"file","path":"config.json","size":10},{"type":"file","path":"a.bin","size":4},{"type":"file","path":"b.bin","size":4},{"type":"file","path":"weights/model.safetensors","size":100},{"type":"file","path":"other/weights/model.safetensors","size":101}]`))
		case strings.HasSuffix(r.URL.Path, "/a.bin") || strings.HasSuffix(r.URL.Path, "/b.bin"):
			<-release
			_, _ = w.Write([]byte("data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()

	s := newTestServerWithConfig(t, Config{CacheDir: t.TempDir(), LocalDir: t.TempDir(), Endpoint: hub.URL, MaxActive: 1, Proxy: &hfdownloader.ProxyConfig{NoEnvProxy: true}})
	mux := http.NewServeMux()
	s.registerAPIRoutes(mux)

	// Request-shape rejections never reach the Hub and admit no job, on both
	// the plan endpoint and the real download endpoint.
	for _, path := range []string{"/api/plan", "/api/download"} {
		for _, tc := range []struct {
			name    string
			req     DownloadRequest
			want    string
			details []string
		}{
			{"empty selection", DownloadRequest{Repo: "owner/model", StrictFiles: true}, "invalid_file_selection", nil},
			{"colon shorthand in repo with a non-empty selection", DownloadRequest{Repo: "owner/model:a.bin", StrictFiles: true, Filters: []string{"a.bin"}}, "invalid_file_selection", []string{"owner/name:file shorthand is not supported"}},
			{"non-canonical path", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"weights/"}}, "invalid_file_selection", nil},
			{"case-colliding entries name both spellings", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"a.bin", "A.bin"}}, "invalid_file_selection", []string{`"a.bin"`, `"A.bin"`}},
			{"combined with excludes", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"a.bin"}, Excludes: []string{"b"}}, "invalid_file_selection", nil},
			{"combined with appendFilterSubdir", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"a.bin"}, AppendFilterSubdir: true}, "invalid_file_selection", nil},
			{"combined with branchVariant", DownloadRequest{Repo: "owner/model", Revision: "4_25", BranchVariant: true, StrictFiles: true, Filters: []string{"a.bin"}}, "invalid_file_selection", nil},
		} {
			code, resp, body := postStrict(mux, path, tc.req)
			if code != http.StatusBadRequest || !strings.Contains(body, tc.want) || resp.ID != "" {
				t.Fatalf("%s %s: status=%d body=%s, want 400 mentioning %q and no admitted job", path, tc.name, code, body, tc.want)
			}
			for _, detail := range tc.details {
				if resp.ErrorDetail == nil || !strings.Contains(resp.ErrorDetail.Details, detail) {
					t.Fatalf("%s %s: body=%s, want error_detail.details to mention %q", path, tc.name, body, detail)
				}
			}
		}
	}
	if got := hubRequests.Load(); got != 0 {
		t.Fatalf("rejected requests reached the hub %d time(s), want none", got)
	}

	// Plan parity: the preview returns exactly the selected files.
	planFiles := func(req DownloadRequest) (int, []string, string) {
		code, _, body := postStrict(mux, "/api/plan", req)
		if code != http.StatusOK {
			return code, nil, body
		}
		var parsed PlanResponse
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, f := range parsed.Files {
			paths = append(paths, f.Path)
		}
		return code, paths, body
	}
	code, planned, body := planFiles(DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"./config.json", "weights/model.safetensors"}})
	if code != http.StatusOK || !reflect.DeepEqual(planned, []string{"config.json", "weights/model.safetensors"}) {
		t.Fatalf("plan status=%d files=%v, want exactly the 2 selected paths; body=%s", code, planned, body)
	}

	// No substitution: the tree lists A.bin before a.bin, so a case-folding
	// matcher would plan A.bin for a selection of a.bin. Exact spelling must
	// plan a.bin and only a.bin.
	code, planned, body = planFiles(DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"a.bin"}})
	if code != http.StatusOK || !reflect.DeepEqual(planned, []string{"a.bin"}) {
		t.Fatalf("case-variant plan status=%d files=%v, want exactly [a.bin]; body=%s", code, planned, body)
	}

	// A well-formed path that does not exist at the revision is a client-side
	// selection problem: plan reports it as 400 no_file_match.
	code, resp, body := postStrict(mux, "/api/plan", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"config.json", "missing.bin"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "no_file_match") {
		t.Fatalf("absent path plan status=%d body=%s, want 400 no_file_match", code, body)
	}
	if resp.ErrorDetail == nil || !strings.Contains(resp.ErrorDetail.Details, "missing.bin") {
		t.Fatalf("absent path error_detail=%+v, want details naming missing.bin; body=%s", resp.ErrorDetail, body)
	}

	// A spelling that differs from the tree's only in case is a miss too, not
	// a silent match of the case-variant sibling: no_file_match names it.
	code, _, body = postStrict(mux, "/api/plan", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"CONFIG.JSON"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "no_file_match") || !strings.Contains(body, "CONFIG.JSON") {
		t.Fatalf("case-variant miss plan status=%d body=%s, want 400 no_file_match naming CONFIG.JSON", code, body)
	}

	// Dedup identity: an identical selection reuses the active job even
	// across spellings; a different selection stays an independent job.
	code, first, body := postStrict(mux, "/api/download", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"./a.bin"}})
	if code != http.StatusAccepted || first.ID == "" {
		t.Fatalf("first admission status=%d body=%s, want 202 with a job id", code, body)
	}
	code, second, body := postStrict(mux, "/api/download", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"a.bin"}})
	if code != http.StatusOK || second.Job == nil || second.Job.ID != first.ID || second.Message != "Download already in progress" {
		t.Fatalf("identical selection status=%d body=%s, want 200 reusing job %s", code, body, first.ID)
	}
	// exactMatch is a documented no-op for strict requests, so flipping it
	// must not create a second job for the same selection.
	code, flipped, body := postStrict(mux, "/api/download", DownloadRequest{Repo: "owner/model", StrictFiles: true, ExactMatch: true, Filters: []string{"a.bin"}})
	if code != http.StatusOK || flipped.Job == nil || flipped.Job.ID != first.ID {
		t.Fatalf("exactMatch-flipped identical selection status=%d body=%s, want 200 reusing job %s", code, body, first.ID)
	}
	code, third, body := postStrict(mux, "/api/download", DownloadRequest{Repo: "owner/model", StrictFiles: true, Filters: []string{"b.bin"}})
	if code != http.StatusAccepted || third.ID == "" || third.ID == first.ID {
		t.Fatalf("different selection status=%d body=%s, want an independent job", code, body)
	}

	// Snapshot exposure: /api/jobs and jobs_state.json both carry the
	// normalized selection and the strict flag.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	var list struct {
		Jobs []*Job `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	listed := findJobByID(list.Jobs, first.ID)
	if listed == nil || !listed.StrictFiles || !reflect.DeepEqual(listed.Filters, []string{"a.bin"}) {
		t.Fatalf("GET /api/jobs snapshot=%+v, want strictFiles with the normalized selection [a.bin]", listed)
	}
	statePath := filepath.Join(t.TempDir(), "jobs_state.json")
	if err := saveJobsState(statePath, s.jobs.ListJobs()); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadJobsState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	restored := findJobByID(loaded, first.ID)
	if restored == nil || !restored.StrictFiles || !reflect.DeepEqual(restored.Filters, []string{"a.bin"}) {
		t.Fatalf("jobs_state.json round trip=%+v, want strictFiles with filters [a.bin]", restored)
	}

	// Both jobs complete only if their plans held exactly their selections:
	// the hub 404s any unplanned path (for example the unselected root
	// companion config.json), which would fail the job instead.
	releaseFn()
	waitJobStatus(t, s.jobs, first.ID, JobStatusCompleted, 15*time.Second)
	waitJobStatus(t, s.jobs, third.ID, JobStatusCompleted, 15*time.Second)
}
