// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func TestAnalyzeAndPlanProxy(t *testing.T) {
	for _, mode := range []string{"explicit", "direct", "no environment", "bypass", "invalid", "unreachable"} {
		t.Run(mode, func(t *testing.T) {
			// ProxyFromEnvironment caches its environment once per process.
			// Isolate this case so earlier tests cannot make the proof vacuous.
			if mode == "no environment" && os.Getenv("HFDESK_PROXY_TEST_CHILD") != "1" {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAnalyzeAndPlanProxy$/^no_environment$", "-test.count=1")
				cmd.Env = append(os.Environ(), "HFDESK_PROXY_TEST_CHILD=1")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("isolated environment case: %v\n%s", err, output)
				}
				return
			}
			var mu sync.Mutex
			var originRequests int
			proxyPaths := map[string]int{}
			serveHub := func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/datasets/"):
					http.NotFound(w, r)
				case strings.Contains(r.URL.Path, "/tree/"):
					io.WriteString(w, `[{"type":"file","path":"config.json","size":2},{"type":"file","path":"model.safetensors","size":100}]`)
				case strings.HasSuffix(r.URL.Path, "/refs"):
					io.WriteString(w, `{"branches":[{"name":"main","targetCommit":"abc"}]}`)
				case strings.HasSuffix(r.URL.Path, "config.json"):
					io.WriteString(w, `{"model_type":"llama"}`)
				case strings.HasSuffix(r.URL.Path, "README.md"):
					io.WriteString(w, "# Model card")
				case r.URL.Path == "/api/models":
					io.WriteString(w, `[{"id":"owner/model"}]`)
				case r.URL.Path == "/api/models/owner/model":
					io.WriteString(w, `{"sha":"abc"}`)
				default:
					http.NotFound(w, r)
				}
			}
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				originRequests++
				mu.Unlock()
				serveHub(w, r)
			}))
			defer hub.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				proxyPaths[r.URL.Path]++
				mu.Unlock()
				serveHub(w, r)
			}))
			defer proxy.Close()
			cfg := Config{Endpoint: hub.URL, ModelsDir: t.TempDir(), CacheDir: t.TempDir()}
			switch mode {
			case "explicit":
				cfg.Endpoint = "http://hub.invalid"
				cfg.Proxy = &hfdownloader.ProxyConfig{URL: proxy.URL, NoEnvProxy: true}
			case "direct":
				cfg.Proxy = nil
			case "no environment":
				t.Setenv("HTTP_PROXY", proxy.URL)
				t.Setenv("NO_PROXY", "")
				t.Setenv("no_proxy", "")
				// localhost. resolves to loopback but is not the special
				// literal localhost bypass in ProxyFromEnvironment.
				cfg.Endpoint = strings.Replace(hub.URL, "127.0.0.1", "localhost.", 1)
				envRequest, err := http.NewRequest("GET", cfg.Endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				envProxy, err := http.ProxyFromEnvironment(envRequest)
				if err != nil || envProxy == nil || envProxy.String() != proxy.URL {
					t.Fatalf("environment fixture is not proxied: %v %v", envProxy, err)
				}
				cfg.Proxy = &hfdownloader.ProxyConfig{NoEnvProxy: true}
				// Unlike environment proxies, NoEnvProxy must bypass non-loopback
				// hosts too; verify the shared production transport's policy.
				client, err := hfdownloader.BuildHTTPClient(cfg.Proxy)
				if err != nil {
					t.Fatal(err)
				}
				if client.Transport.(*http.Transport).Proxy != nil {
					t.Fatal("environment proxy still enabled")
				}
			case "bypass":
				cfg.Proxy = &hfdownloader.ProxyConfig{URL: proxy.URL, NoProxy: "127.0.0.1", NoEnvProxy: true}
			case "invalid":
				cfg.Proxy = &hfdownloader.ProxyConfig{URL: "://bad", NoEnvProxy: true}
			case "unreachable":
				// Destination port zero cannot host a listener: binding port zero
				// allocates an ephemeral port instead. Unlike a closed test server's
				// port, this destination cannot be reused by another test's listener.
				cfg.Proxy = &hfdownloader.ProxyConfig{URL: "http://127.0.0.1:0", NoEnvProxy: true}
			}
			s := newTestServerWithConfig(t, cfg)
			for _, action := range []string{"analyze", "plan", "dry-run", "readme", "search"} {
				if (mode == "invalid" || mode == "unreachable") && (action == "readme" || action == "search") {
					continue
				}
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", "/api/"+action+"/owner/model", nil)
				r.SetPathValue("repo", "owner/model")
				switch action {
				case "analyze":
					s.handleAnalyze(w, r)
				case "plan":
					s.handlePlan(w, httptest.NewRequest("POST", "/api/plan", strings.NewReader(`{"repo":"owner/model"}`)))
				case "dry-run":
					s.handleStartDownload(w, httptest.NewRequest("POST", "/api/download", strings.NewReader(`{"repo":"owner/model","dryRun":true}`)))
				case "readme":
					s.handleReadme(w, r)
				case "search":
					s.handleSearch(w, r)
				}
				if mode == "invalid" || mode == "unreachable" {
					if w.Code != 500 || !strings.Contains(w.Body.String(), "proxy") {
						t.Fatalf("%s error hidden: %d %s", action, w.Code, w.Body)
					}
				} else {
					if w.Code != 200 {
						t.Fatalf("%s: %d %s", action, w.Code, w.Body)
					}
					var response map[string]interface{}
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if action == "analyze" && (response["type"] != "transformers" || response["metadata"].(map[string]interface{})["config.json"] == nil || len(response["refs"].([]interface{})) != 1) {
						t.Fatalf("incomplete analysis: %s", w.Body)
					}
					if (action == "plan" || action == "dry-run") && response["totalFiles"] != float64(2) {
						t.Fatalf("missing plan files: %s", w.Body)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if (mode == "invalid" || mode == "unreachable") && originRequests != 0 {
				t.Fatalf("proxy failure bypassed to origin: %d requests", originRequests)
			}
			if mode == "explicit" {
				for _, path := range []string{"/api/models/owner/model/tree/main", "/api/datasets/owner/model/tree/main", "/api/models/owner/model/refs", "/owner/model/raw/main/config.json", "/owner/model/raw/main/README.md", "/api/models"} {
					if proxyPaths[path] == 0 {
						t.Errorf("request did not reach proxy: %s", path)
					}
				}
			} else if len(proxyPaths) != 0 {
				t.Fatalf("unexpected proxy requests: %v", proxyPaths)
			}
		})
	}
}

func TestAnalyzeMetadataErrorVisible(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/datasets/"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/tree/"):
			io.WriteString(w, `[{"type":"file","path":"config.json"},{"type":"file","path":"model.safetensors"},{"type":"file","path":"quantization_config.json"}]`)
		case strings.HasSuffix(r.URL.Path, "quantization_config.json"):
			io.WriteString(w, `{"layers":["`+strings.Repeat("x", 10*1024*1024)+`"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	s := newTestServerWithConfig(t, Config{Endpoint: hub.URL, Proxy: &hfdownloader.ProxyConfig{NoEnvProxy: true}, CacheDir: t.TempDir()})
	r := httptest.NewRequest("GET", "/api/analyze/owner/model", nil)
	r.SetPathValue("repo", "owner/model")
	w := httptest.NewRecorder()
	s.handleAnalyze(w, r)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "quantization_config.json") || !strings.Contains(w.Body.String(), "10 MiB") {
		t.Fatalf("metadata error not visible: %d %s", w.Code, w.Body)
	}
}
