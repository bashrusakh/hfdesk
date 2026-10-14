// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

type cacheSelectionMember struct {
	Path     string                 `json:"path"`
	Versions []string               `json:"versions,omitempty"`
	Size     int64                  `json:"size"`
	LinkOnly bool                   `json:"linkOnly,omitempty"`
	Message  string                 `json:"message,omitempty"`
	identity *selectedEntryIdentity `json:"-"`
}

type cacheSelectionGroup struct {
	ID        string                 `json:"id"`
	Label     string                 `json:"label"`
	Quant     string                 `json:"quant,omitempty"`
	Members   []cacheSelectionMember `json:"members"`
	Warning   string                 `json:"warning,omitempty"`
	CanDelete bool                   `json:"canDelete"`
}

type cacheSelectionLocation struct {
	ID           string                `json:"id"`
	Source       string                `json:"source"`
	Path         string                `json:"path"`
	Groups       []cacheSelectionGroup `json:"groups"`
	Warning      string                `json:"warning,omitempty"`
	CanDelete    bool                  `json:"canDelete"`
	DeleteReason string                `json:"deleteReason,omitempty"`
	kind         string                `json:"-"`
}

type cacheSelectionFile struct {
	path, version string
	size          int64
	linkOnly      bool
	identity      *selectedEntryIdentity
}

type selectedEntryIdentity struct {
	info os.FileInfo
	pin  *os.File
}

var cacheShardSuffix = regexp.MustCompile(`(?i)([-_])(\d+)([-_]of[-_])(\d+)(\.gguf)$`)

func selectionHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func selectionFamily(relative string) (family, quant string, splitIndex, splitTotal int) {
	dir, base := filepath.Split(filepath.ToSlash(relative))
	quant = cacheQuantLabel(strings.TrimSuffix(base, filepath.Ext(base)))
	identityBase := base
	if loc := cacheShardSuffix.FindStringSubmatchIndex(base); len(loc) == 12 {
		splitIndex, _ = strconv.Atoi(base[loc[4]:loc[5]])
		splitTotal, _ = strconv.Atoi(base[loc[8]:loc[9]])
		if splitIndex == 0 && splitTotal == 0 {
			splitTotal = -1
		}
		identityBase = base[:loc[4]] + "{part}" + base[loc[5]:]
	}
	return filepath.ToSlash(filepath.Join(dir, identityBase)), quant, splitIndex, splitTotal
}

func makeSelectionGroups(locationID string, files []cacheSelectionFile) []cacheSelectionGroup {
	type aggregate struct {
		family, quant string
		members       map[string]*cacheSelectionMember
		indices       map[string]map[int]bool
		expected      map[string]int
	}
	aggs := map[string]*aggregate{}
	for _, file := range files {
		if !strings.EqualFold(filepath.Ext(file.path), ".gguf") || isCacheMMProjFile(file.path) {
			continue
		}
		family, quant, index, total := selectionFamily(file.path)
		if quant == "" {
			parentQuant := cacheQuantLabel(filepath.Base(filepath.Dir(file.path)))
			quant = parentQuant
		}
		shape := "single"
		isShard := index != 0 || total != 0
		if isShard {
			shape = "split"
		}
		key := family + "\x00" + shape
		a := aggs[key]
		if a == nil {
			a = &aggregate{family: family, quant: quant, members: map[string]*cacheSelectionMember{}, indices: map[string]map[int]bool{}, expected: map[string]int{}}
			aggs[key] = a
		}
		memberKey := file.path + "\x00" + file.version
		m := a.members[memberKey]
		if m == nil {
			m = &cacheSelectionMember{Path: file.path, Size: file.size, LinkOnly: file.linkOnly, identity: file.identity}
			if file.linkOnly {
				m.Message = "Only this link is represented; its weight target will remain"
			}
			a.members[memberKey] = m
		}
		if file.version != "" {
			m.Versions = []string{file.version}
		}
		if isShard {
			if a.indices[file.version] == nil {
				a.indices[file.version] = map[int]bool{}
			}
			a.indices[file.version][index] = true
			a.expected[file.version] = total
			if total <= 0 || index <= 0 || index > total {
				a.indices[file.version][-1] = true
			}
		}
	}
	keys := make([]string, 0, len(aggs))
	for key := range aggs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]cacheSelectionGroup, 0, len(keys))
	for _, key := range keys {
		a := aggs[key]
		members := make([]cacheSelectionMember, 0, len(a.members))
		for _, m := range a.members {
			members = append(members, *m)
		}
		sort.Slice(members, func(i, j int) bool {
			return members[i].Path+strings.Join(members[i].Versions, "") < members[j].Path+strings.Join(members[j].Versions, "")
		})
		label := a.family
		if a.quant != "" {
			label += " · " + a.quant
		}
		g := cacheSelectionGroup{ID: "gguf-" + selectionHash(locationID, key), Label: label, Quant: a.quant, Members: members}
		versions := make([]string, 0, len(a.expected))
		for version := range a.expected {
			versions = append(versions, version)
		}
		sort.Strings(versions)
		for _, version := range versions {
			expected := a.expected[version]
			if a.indices[version][-1] || (expected > 0 && len(a.indices[version]) != expected) {
				g.Warning = fmt.Sprintf("Shard set appears incomplete in %s: found %d of %d numbered parts", versionLabel(version), len(a.indices[version]), expected)
				break
			}
		}
		out = append(out, g)
	}
	return out
}

func versionLabel(version string) string {
	if version == "" {
		return "this location"
	}
	return "saved version " + version
}

func localSelectionFiles(repoDir string, excluded []string) ([]cacheSelectionFile, string) {
	var files []cacheSelectionFile
	warning := ""
	err := walkLocalCacheRepo(repoDir, excluded, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.EqualFold(filepath.Ext(info.Name()), ".gguf") {
			return nil
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		size, linkOnly := info.Size(), info.Mode()&os.ModeSymlink != 0
		if linkOnly {
			actual, evalErr := os.Stat(path)
			if evalErr != nil || !actual.Mode().IsRegular() {
				warning = "A GGUF link could not be resolved; unavailable links are omitted"
				return nil
			}
			size = actual.Size()
		} else if !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, cacheSelectionFile{path: filepath.ToSlash(rel), size: size, linkOnly: linkOnly})
		return nil
	})
	if err != nil {
		return files, "Could not fully read this location: " + err.Error()
	}
	return files, warning
}

func hfSelectionFiles(rd *hfdownloader.RepoDir, protectedRoots []string) ([]cacheSelectionFile, string) {
	snapshots, err := rd.ListSnapshots()
	if err != nil {
		return nil, "Could not list saved versions: " + err.Error()
	}
	var files []cacheSelectionFile
	warnings := []string{}
	if len(snapshots) == 0 {
		if entries, readErr := os.ReadDir(rd.BlobsDir()); readErr == nil && len(entries) > 0 {
			warnings = append(warnings, "No named snapshot entries are available to identify GGUF files")
		} else if readErr != nil && !os.IsNotExist(readErr) {
			warnings = append(warnings, "Could not inspect HF blobs: "+readErr.Error())
		}
	}
	for _, version := range snapshots {
		dir, err := rd.SnapshotDir(version)
		if err != nil {
			warnings = append(warnings, "A saved version has an invalid path")
			continue
		}
		err = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			for _, root := range protectedRoots {
				if withinLocalRoot(root, path) {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if info.IsDir() || !strings.EqualFold(filepath.Ext(info.Name()), ".gguf") {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			// Require a readable target file; dangling snapshot links are not available weights.
			actual, err := os.Stat(path)
			if err != nil {
				if info.Mode()&os.ModeSymlink != 0 && os.IsNotExist(err) {
					warnings = append(warnings, "A GGUF snapshot link is unavailable in saved version "+version)
				} else if !os.IsNotExist(err) {
					warnings = append(warnings, "Could not inspect GGUF in saved version "+version+": "+err.Error())
				}
				return nil
			}
			if !actual.Mode().IsRegular() {
				return nil
			}
			files = append(files, cacheSelectionFile{path: filepath.ToSlash(rel), version: version, size: actual.Size()})
			return nil
		})
		if err != nil {
			warnings = append(warnings, "Could not fully read saved version "+version)
		}
	}
	return files, strings.Join(warnings, "; ")
}

// configuredHFProtectedRoots maps explicit configured local roots that fall
// inside this repository's snapshot or friendly tree to lexical walk boundaries.
// The implicit cache and models roots are deliberately not included. A false
// completeness result means at least one configured root's identity could not
// be established, so HF deletion must be refused even though known boundaries
// remain available for best-effort listing.
func configuredHFProtectedRoots(cfg Config, rd *hfdownloader.RepoDir) ([]string, bool) {
	configured := append([]string(nil), cfg.LocalScanDirs...)
	configured = append(configured, cfg.LocalDir)
	configured = append(configured, routeDirs(cfg.DownloadRoutes)...)
	type protectedTree struct {
		lexical  string
		physical string
	}
	trees := make([]protectedTree, 0, 2)
	complete := true
	for _, tree := range []string{rd.SnapshotsDir(), rd.FriendlyPath()} {
		lexical, physical, ok := configuredRootIdentity(tree)
		if !ok {
			complete = false
			continue
		}
		trees = append(trees, protectedTree{lexical: lexical, physical: physical})
	}
	var protected []string
	seen := map[string]bool{}
	add := func(boundary string) {
		key := pathIdentityKey(filepath.Clean(boundary))
		if !seen[key] {
			seen[key] = true
			protected = append(protected, key)
		}
	}
	for _, root := range configured {
		if root == "" {
			continue
		}
		rootLexical, rootPhysical, rootOK := configuredRootIdentity(root)
		if !rootOK {
			complete = false
			// Preserve a provable lexical boundary for best-effort display. If the
			// configured alias is lexically elsewhere, its physical relationship
			// cannot be inferred and deletion remains disabled by complete=false.
			if rootLexical != "" {
				for _, tree := range trees {
					if withinLocalRoot(tree.lexical, rootLexical) {
						add(rootLexical)
					}
				}
			}
			continue
		}
		for _, tree := range trees {
			if rootPhysical != tree.physical && !withinLocalRoot(tree.physical, rootPhysical) {
				continue
			}
			rel, err := filepath.Rel(tree.physical, rootPhysical)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				complete = false
				continue
			}
			boundary := tree.lexical
			if rel != "." {
				boundary = filepath.Join(tree.lexical, rel)
			}
			add(boundary)
		}
	}
	return protected, complete
}

func hfDeletionScopeWarning(rd *hfdownloader.RepoDir) string {
	for _, name := range []string{filepath.Dir(rd.Path()), rd.Path()} {
		abs, err := filepath.Abs(name)
		if err != nil {
			return "HF repository path could not be resolved safely"
		}
		for cur := abs; ; cur = filepath.Dir(cur) {
			info, err := os.Lstat(cur)
			if err != nil {
				return "HF repository path could not be inspected safely"
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "HF repository path contains an unsafe directory alias"
			}
			if filepath.Dir(cur) == cur {
				break
			}
		}
	}
	snapshots := filepath.Join(rd.Path(), "snapshots")
	blobs := filepath.Join(rd.Path(), "blobs")
	if info, err := os.Lstat(blobs); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "HF blobs directory is unsafe or unavailable"
	}
	if info, err := os.Lstat(snapshots); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "HF snapshots directory is unsafe"
		}
		blobRoot := blobs
		err = filepath.WalkDir(snapshots, func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name != snapshots && entry.Type()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink == 0 {
				return nil
			}
			target, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(name), target)
			}
			target, err = filepath.Abs(filepath.Clean(target))
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(blobRoot, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return fmt.Errorf("snapshot link escapes repository blobs")
			}
			return nil
		})
		if err != nil {
			return "HF snapshot references could not be safely enumerated"
		}
	} else if !os.IsNotExist(err) {
		return "HF snapshots directory could not be inspected"
	}
	// The friendly tree is independently mutable. Reject symlinked directory
	// aliases and incomplete enumeration before advertising the mutation.
	friendly := rd.FriendlyPath()
	if _, err := os.Lstat(friendly); os.IsNotExist(err) {
		return ""
	} else if err != nil {
		return "Friendly view could not be inspected safely"
	}
	for cur := friendly; ; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err != nil {
			return "Friendly view could not be inspected safely"
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "Friendly view contains an unsafe directory alias"
		}
		if filepath.Dir(cur) == cur {
			break
		}
	}
	err := filepath.WalkDir(friendly, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			_, err = os.Readlink(name)
			return err
		}
		return nil
	})
	if err != nil {
		return "Friendly view could not be completely enumerated"
	}
	return ""
}

func isRegisteredLocalSelectionRoot(cfg Config, candidate string) bool {
	known := []string{filepath.Join(cfg.cacheRoot(), "models"), cfg.cacheRoot()}
	if cfg.LocalDir != "" {
		known = append(known, cfg.LocalDir)
	}
	known = append(known, cfg.LocalScanDirs...)
	known = append(known, routeDirs(cfg.DownloadRoutes)...)
	want, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return false
	}
	want = pathIdentityKey(want)
	for _, root := range known {
		if root == "" {
			continue
		}
		got, err := filepath.Abs(filepath.Clean(root))
		if err == nil && pathIdentityKey(got) == want {
			return true
		}
	}
	return false
}

func cacheSelectionLocations(cfg Config, repo string) []cacheSelectionLocation {
	cache := cfg.cache()
	roots := localCacheRoots(cfg.cacheRoot(), cfg.LocalDir, cfg.LocalScanDirs, cfg.DownloadRoutes)
	locations := []cacheSelectionLocation{}
	for _, root := range roots {
		if root.skipsOwner(strings.SplitN(repo, "/", 2)[0]) {
			continue
		}
		parts := strings.SplitN(repo, "/", 2)
		dir := filepath.Join(root.Path, parts[0], parts[1])
		st, err := os.Stat(dir)
		if os.IsNotExist(err) || (err == nil && !st.IsDir()) {
			continue
		}
		id := "local-" + selectionHash(filepath.Clean(root.Path), repo, "model")
		allowed := isRegisteredLocalSelectionRoot(cfg, root.Path)
		// A registered root authorizes only paths beneath that root. Do not
		// turn an owner/model symlink into a new deletion root.
		if err == nil {
			for _, component := range []string{filepath.Join(root.Path, parts[0]), dir} {
				componentInfo, componentErr := os.Lstat(component)
				if componentErr != nil || componentInfo.Mode()&os.ModeSymlink != 0 || !componentInfo.IsDir() {
					allowed = false
				}
			}
		}
		if err != nil {
			locations = append(locations, cacheSelectionLocation{ID: id, Source: root.Source, Path: dir, Warning: "Could not inspect this configured location: " + err.Error(), DeleteReason: "Configured location could not be inspected", kind: "local"})
			continue
		}
		excluded, boundariesComplete := root.excludedSubrootsWithStatus(roots)
		files, warning := localSelectionFiles(dir, excluded)
		if !boundariesComplete {
			if warning != "" {
				warning += "; "
			}
			warning += "A configured scan-root boundary could not be safely resolved"
		}
		groups := makeSelectionGroups(id, files)
		for i := range groups {
			groups[i].CanDelete = allowed && warning == ""
		}
		if len(groups) == 0 && warning == "" {
			continue
		}
		loc := cacheSelectionLocation{ID: id, Source: root.Source, Path: dir, Groups: groups, Warning: warning, CanDelete: allowed && warning == "", kind: "local"}
		if !allowed {
			loc.DeleteReason = "This location is not a currently configured local scan or download root"
		} else if warning != "" {
			loc.DeleteReason = "This location could not be completely enumerated"
		}
		locations = append(locations, loc)
	}
	if rd, err := cache.Repo(repo, hfdownloader.RepoTypeModel); err == nil {
		id := "hf-" + selectionHash(filepath.Clean(cache.HubDir()), repo, "model")
		if info, statErr := os.Stat(rd.Path()); statErr == nil && info.IsDir() {
			protectedRoots, protectionComplete := configuredHFProtectedRoots(cfg, rd)
			files, warning := hfSelectionFiles(rd, protectedRoots)
			if !protectionComplete {
				if warning != "" {
					warning += "; "
				}
				warning += "A configured local root could not be safely resolved against HF storage"
			}
			if scopeWarning := hfDeletionScopeWarning(rd); scopeWarning != "" {
				if warning != "" {
					warning += "; "
				}
				warning += scopeWarning
			}
			groups := makeSelectionGroups(id, files)
			for i := range groups {
				groups[i].CanDelete = warning == ""
			}
			if len(groups) > 0 || warning != "" {
				reason := ""
				if warning != "" {
					reason = "HF snapshots could not be completely inspected"
				}
				locations = append(locations, cacheSelectionLocation{ID: id, Source: "HF cache", Path: rd.Path(), Groups: groups, Warning: warning, CanDelete: warning == "", DeleteReason: reason, kind: "hf"})
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			locations = append(locations, cacheSelectionLocation{ID: id, Source: "HF cache", Path: rd.Path(), Groups: []cacheSelectionGroup{}, Warning: "Could not inspect this HF location: " + statErr.Error(), DeleteReason: "Selected HF-cache deletion is not supported", kind: "hf"})
		} else if statErr == nil {
			locations = append(locations, cacheSelectionLocation{ID: id, Source: "HF cache", Path: rd.Path(), Groups: []cacheSelectionGroup{}, Warning: "HF repository path is not a directory", DeleteReason: "Selected HF-cache deletion is not supported", kind: "hf"})
		}
	}
	return locations
}

func (s *Server) handleCacheSelection(w http.ResponseWriter, r *http.Request) {
	repo, repoType := r.URL.Query().Get("repo"), r.URL.Query().Get("type")
	if !hfdownloader.IsValidModelName(repo) {
		writeError(w, http.StatusBadRequest, "Invalid repo format", "Expected repo=owner/name")
		return
	}
	if repoType != "model" && repoType != "dataset" {
		writeError(w, http.StatusBadRequest, "Invalid repository type", "Expected type=model or type=dataset")
		return
	}
	if repoType == "dataset" {
		writeError(w, http.StatusBadRequest, "GGUF selection is only available for models", "Dataset cache behavior is unchanged")
		return
	}
	cfg := s.snapshotConfig()
	locations := cacheSelectionLocations(cfg, repo)
	selected := r.URL.Query().Get("locationId")
	if selected != "" {
		for _, location := range locations {
			if location.ID == selected {
				writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "type": repoType, "locations": []cacheSelectionLocation{location}})
				return
			}
		}
		writeError(w, http.StatusBadRequest, "Unknown cache location", "Refresh the selection list and choose a currently configured location")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "type": repoType, "locations": locations})
}
