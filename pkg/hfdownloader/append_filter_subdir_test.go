// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownload_NonLFSModelFilterDoesNotChangeDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/revision/main"):
			_ = json.NewEncoder(w).Encode(RepoInfo{SHA: "deadbeef"})
		case strings.Contains(r.URL.Path, "/tree/"):
			_ = json.NewEncoder(w).Encode([]hfNode{{Type: "file", Path: "config.json", Size: 4}})
		case r.URL.Path == "/owner/model/raw/main/config.json":
			_, _ = w.Write([]byte("data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	for _, tc := range []struct {
		name     string
		append   bool
		rootFile bool
	}{
		{name: "append with existing root file", append: true, rootFile: true},
		{name: "append into clean target", append: true},
		{name: "append disabled control", append: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			base, err := DestinationBase(out, "owner/model")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(base, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.rootFile {
				if err := os.WriteFile(filepath.Join(base, "config.json"), []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err = Download(context.Background(), Job{
				Repo: "owner/model", Revision: "main", Filters: []string{"config.json"}, AppendFilterSubdir: tc.append,
			}, Settings{Endpoint: server.URL, OutputDir: out, Concurrency: 1, MaxActiveDownloads: 1}, nil)
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(base, "config.json"))
			if err != nil {
				t.Fatalf("read destination config.json: %v", err)
			}
			if string(got) != "data" {
				t.Errorf("config.json = %q, want downloaded content", got)
			}
		})
	}
}
