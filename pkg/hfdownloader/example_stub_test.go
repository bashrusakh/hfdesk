// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// exampleStubCommit is the fixed commit the local Hub stub reports for every
// revision lookup, pinning every plan to one immutable revision.
const exampleStubCommit = "deadbeef"

// exampleStubFile is one file served by a stub repository.
type exampleStubFile struct {
	Path string
	Size int64
	LFS  bool
}

// exampleStubRepos holds the tiny deterministic repositories the local Hub
// stub serves. The Example functions resolve against these fixtures, so they
// run hermetically offline with stable output; sizes are fixture sizes, not
// real repository contents.
var exampleStubRepos = map[string][]exampleStubFile{
	"hf-internal-testing/tiny-random-gpt2": {
		{Path: "config.json", Size: 647},
	},
	"TheBloke/Mistral-7B-Instruct-v0.2-GGUF": {
		{Path: "mistral-7b-instruct-v0.2.Q4_K_M.gguf", Size: 1024, LFS: true},
		{Path: "mistral-7b-instruct-v0.2.Q5_K_M.gguf", Size: 768, LFS: true},
		{Path: "mistral-7b-instruct-v0.2.Q6_K.gguf", Size: 512, LFS: true},
	},
	"facebook/flores": {
		{Path: "dataset_infos.json", Size: 256},
		{Path: "dev.dev", Size: 128},
	},
	"TheBloke/Llama-2-7B-GGUF": {
		{Path: "llama-2-7b.Q4_0.gguf", Size: 2048, LFS: true},
		{Path: "llama-2-7b.Q5_0.gguf", Size: 1536, LFS: true},
		{Path: "llama-2-7b.Q6_K.gguf", Size: 1024, LFS: true},
	},
	"meta-llama/Llama-2-7b": {
		{Path: "pytorch_model.bin", Size: 512, LFS: true},
	},
}

// exampleHub is a local stub of the Hugging Face Hub: the revision lookup,
// tree listing and raw/resolve file downloads the Example functions perform.
// It starts once per test binary and exits with the process.
var exampleHub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.Contains(p, "/revision/"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": exampleStubCommit})
	case strings.Contains(p, "/tree/"):
		files := exampleStubRepos[exampleRepoID(p, "/tree/")]
		nodes := make([]map[string]any, 0, len(files))
		for _, f := range files {
			node := map[string]any{"type": "file", "path": f.Path, "size": f.Size}
			if f.LFS {
				node["lfs"] = map[string]any{"size": f.Size}
			}
			nodes = append(nodes, node)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
	default:
		// File download: {repo}/raw/{rev}/{path} or {repo}/resolve/{rev}/{path},
		// each optionally under a datasets/ prefix.
		rest := strings.TrimPrefix(p, "/")
		rest = strings.TrimPrefix(rest, "datasets/")
		marker := "/raw/"
		if strings.Contains(rest, "/resolve/") {
			marker = "/resolve/"
		}
		i := strings.Index(rest, marker)
		if i < 0 {
			http.NotFound(w, r)
			return
		}
		rel := rest[i+len(marker):]
		if j := strings.IndexByte(rel, '/'); j >= 0 {
			rel = rel[j+1:] // drop the revision segment
		}
		for _, f := range exampleStubRepos[rest[:i]] {
			if f.Path == rel {
				_, _ = w.Write(exampleStubBody(f.Size))
				return
			}
		}
		http.NotFound(w, r)
	}
}))

// exampleEndpoint returns the local Hub stub's URL for Settings.Endpoint.
func exampleEndpoint() string {
	return exampleHub.URL
}

// exampleRepoID extracts the repository id from an API path such as
// /api/models/owner/repo/tree/<rev> or /api/datasets/owner/repo/tree/<rev>.
func exampleRepoID(p, marker string) string {
	p = strings.TrimPrefix(p, "/api/models/")
	p = strings.TrimPrefix(p, "/api/datasets/")
	if i := strings.Index(p, marker); i >= 0 {
		return p[:i]
	}
	return ""
}

// exampleStubBody returns size deterministic filler bytes for a fixture file.
func exampleStubBody(size int64) []byte {
	const pattern = "hfdesk-example-payload:"
	n := (size + int64(len(pattern)) - 1) / int64(len(pattern))
	return bytes.Repeat([]byte(pattern), int(n))[:size]
}

// exampleOutputDir returns a temporary output directory and a cleanup
// function so examples download into an isolated location and leave no
// files behind.
func exampleOutputDir() (string, func()) {
	dir, err := os.MkdirTemp("", "hfdesk-example-")
	if err != nil {
		panic(err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }
}

// exampleRelativeFiles lists the regular files under root as sorted,
// slash-separated paths relative to root, so examples can print the exact
// destinations they produced deterministically on every platform.
func exampleRelativeFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}
