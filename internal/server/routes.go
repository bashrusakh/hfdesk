// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
)

// errInvalidRouteKey is returned by CreateJob when a request supplies a
// routeKey outside the closed, server-defined key set. Handlers map it to
// HTTP 400 rather than treating the value as a destination path.
var errInvalidRouteKey = errors.New("invalid routeKey")

// routeKeys is the closed, server-owned set of download-route keys. Keys are
// an internal/advanced-API detail; the dashboard renders human labels for
// them. The set is not user-extensible: any key outside it is rejected on the
// settings and download APIs.
var routeKeys = map[string]bool{
	"llm":             true,
	"llm/gguf":        true,
	"llm/safetensors": true,
	"diffusion":       true,
	"audio":           true,
	"embedding":       true,
}

// isRouteKey reports whether key belongs to the closed route-key set.
func isRouteKey(key string) bool {
	return routeKeys[key]
}

// parentRouteKey returns the coarse parent of a namespaced route key
// ("llm/gguf" -> "llm"), or "" when the key has no parent. Used for
// most-specific-first resolution.
func parentRouteKey(key string) string {
	i := strings.LastIndex(key, "/")
	if i <= 0 {
		return ""
	}
	return key[:i]
}

// resolveRoute returns the configured destination for a route key using
// most-specific-first fallback: "llm/gguf" tries "llm/gguf" then "llm";
// a top-level key tries only itself. It returns "" when the key is unknown or
// no route (including the parent) is configured. Empty or whitespace-only
// values are ignored; non-empty values are trimmed and cleaned.
func resolveRoute(routes map[string]string, key string) string {
	if len(routes) == 0 || !isRouteKey(key) {
		return ""
	}
	for k := key; k != ""; k = parentRouteKey(k) {
		v, ok := routes[k]
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		return filepath.Clean(v)
	}
	return ""
}

// normalizeDownloadRoutes trims each value, filepath.Cleans it, drops empty
// entries, and returns nil for an empty result. It mirrors cleanPathList
// semantics for a keyed map. Keys are assumed already validated.
func normalizeDownloadRoutes(routes map[string]string) map[string]string {
	if len(routes) == 0 {
		return nil
	}
	out := make(map[string]string, len(routes))
	for key, value := range routes {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out[key] = filepath.Clean(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitizeDownloadRoutes drops every key outside the closed route-key set and
// then applies normalizeDownloadRoutes value semantics (trim, filepath.Clean,
// drop empties). It is the shared boundary filter for route maps so the server
// never holds or advertises a key outside the closed set: a hand-edited or
// legacy config key is silently dropped here rather than surviving in
// cfg.DownloadRoutes and failing later validation. Returns nil when no valid
// non-empty entry remains. Values dropped for an unknown key are never
// interpreted as paths.
func sanitizeDownloadRoutes(routes map[string]string) map[string]string {
	if len(routes) == 0 {
		return nil
	}
	known := make(map[string]string, len(routes))
	for key, value := range routes {
		if !isRouteKey(key) {
			continue
		}
		known[key] = value
	}
	return normalizeDownloadRoutes(known)
}

// routeDirs returns the distinct, non-empty destination paths from a
// download-routes map, cleaned and sorted for a deterministic scan order.
// Only keys in the closed route-key set are considered, so a hand-edited
// config file cannot widen the scan/disk-free allowlist with unknown keys.
// Used to make routed destinations visible to the Cache browser and the
// disk-free allowlist.
func routeDirs(routes map[string]string) []string {
	if len(routes) == 0 {
		return nil
	}
	var dirs []string
	seen := make(map[string]bool, len(routes))
	for key, value := range routes {
		if !isRouteKey(key) {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		cleaned := filepath.Clean(value)
		norm := strings.ToLower(cleaned)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		dirs = append(dirs, cleaned)
	}
	sort.Strings(dirs)
	return dirs
}
