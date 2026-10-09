// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// SelectedGGUFEntry identifies a named snapshot entry confirmed by the caller.
type SelectedGGUFEntry struct {
	Path     string
	Versions []string
}

// SelectedGGUFDeleteResult reports actual entry removals and payloads retained
// because another named snapshot entry still refers to them.
type SelectedGGUFDeleteResult struct {
	Removed, Remaining, RetainedBlobs []string
	Errors                            []string
	Attempted                         bool
}

func cleanHFRelative(name string) bool {
	return name != "" && !strings.Contains(name, `\`) && !path.IsAbs(name) && !(len(name) >= 2 && name[1] == ':') && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}

func hfLinkTarget(link string) (string, error) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	return filepath.Clean(target), nil
}

func pathWithin(base, candidate string) bool {
	rel, err := filepath.Rel(base, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// physicalEntryIdentity resolves only parent directories, preserving the final
// filename as an entry. This follows directory aliases without following a leaf
// symlink, so reference checks identify the actual link/file name being retained.
func physicalEntryIdentity(name string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	missing := []string{}
	var resolved string
	for {
		resolved, err = filepath.EvalSymlinks(parent)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		missing = append(missing, filepath.Base(parent))
		parent = next
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	identity := filepath.Clean(filepath.Join(resolved, filepath.Base(abs)))
	if runtime.GOOS == "windows" {
		identity = strings.ToLower(identity)
	}
	return identity, nil
}

func safeDirectoryChain(name string) error {
	abs, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	cur := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(abs, volume), string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, e := os.Lstat(cur)
		if e != nil {
			return e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe directory component")
		}
	}
	return nil
}

// DeleteSelectedGGUF removes only confirmed names from all named snapshots,
// their provably associated friendly links, and blob payloads with no remaining
// direct snapshot references. It never removes directories or partial files.
func (r *RepoDir) DeleteSelectedGGUF(entries []SelectedGGUFEntry, protectedRoots ...string) SelectedGGUFDeleteResult {
	result := SelectedGGUFDeleteResult{Removed: []string{}, Remaining: []string{}, RetainedBlobs: []string{}, Errors: []string{}}
	if len(entries) == 0 {
		result.Errors = append(result.Errors, "empty selection")
		return result
	}
	repo, err := filepath.Abs(r.Path())
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	// Refuse repository path aliases and unsafe roots before inspecting entries.
	for _, p := range []string{r.cache.HubDir(), repo} {
		cur, e := filepath.Abs(p)
		if e != nil {
			result.Errors = append(result.Errors, e.Error())
			return result
		}
		for cur != filepath.Dir(cur) {
			info, e := os.Lstat(cur)
			if e != nil {
				result.Errors = append(result.Errors, e.Error())
				return result
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				result.Errors = append(result.Errors, "unsafe HF repository path")
				return result
			}
			cur = filepath.Dir(cur)
		}
	}
	snapshotRoot := r.SnapshotsDir()
	blobRoot := r.BlobsDir()
	friendlyRoot := r.FriendlyPath()
	if err := safeDirectoryChain(repo); err != nil {
		result.Errors = append(result.Errors, "unsafe HF repository path: "+err.Error())
		return result
	}
	snapshotInfo, err := os.Lstat(snapshotRoot)
	if err != nil || !snapshotInfo.IsDir() || snapshotInfo.Mode()&os.ModeSymlink != 0 {
		result.Errors = append(result.Errors, "unsafe or unreadable snapshots directory")
		return result
	}
	blobInfo, err := os.Lstat(blobRoot)
	if err != nil || !blobInfo.IsDir() || blobInfo.Mode()&os.ModeSymlink != 0 {
		result.Errors = append(result.Errors, "unsafe or unreadable blobs directory")
		return result
	}
	snapshotEntries, err := os.ReadDir(snapshotRoot)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	snapshots := make([]string, 0, len(snapshotEntries))
	for _, entry := range snapshotEntries {
		info, e := os.Lstat(filepath.Join(snapshotRoot, entry.Name()))
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			result.Errors = append(result.Errors, "unsafe snapshot entry")
			return result
		}
		snapshots = append(snapshots, entry.Name())
	}
	sort.Strings(snapshots)
	selected := map[string]bool{}
	selectedRemotePaths := map[string]string{}
	cleanProtectedRoots := make([]string, 0, len(protectedRoots))
	for _, root := range protectedRoots {
		root, err = filepath.Abs(filepath.Clean(root))
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			return result
		}
		cleanProtectedRoots = append(cleanProtectedRoots, root)
	}
	for _, item := range entries {
		if !cleanHFRelative(item.Path) || !strings.EqualFold(path.Ext(item.Path), ".gguf") {
			result.Errors = append(result.Errors, "unsafe or non-GGUF selected path")
			return result
		}
		for _, version := range item.Versions {
			if !cleanHFRelative(version) {
				result.Errors = append(result.Errors, "unsafe snapshot version")
				return result
			}
			name := filepath.Join(snapshotRoot, version, filepath.FromSlash(item.Path))
			for _, root := range cleanProtectedRoots {
				if pathWithin(root, name) {
					result.Errors = append(result.Errors, "selected snapshot entry belongs to a configured local root")
					return result
				}
			}
			selected[name] = true
			selectedRemotePaths[name] = item.Path
		}
	}
	if len(selected) == 0 {
		result.Errors = append(result.Errors, "selection has no saved-version entries")
		return result
	}
	// Complete bounded enumeration is required before any unlink.
	refs := map[string]map[string]bool{}
	for _, version := range snapshots {
		root := filepath.Join(snapshotRoot, version)
		err = filepath.WalkDir(root, func(name string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				if name != root && d.Type()&os.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				return nil
			}
			info, e := os.Lstat(name)
			if e != nil {
				return e
			}
			if info.Mode()&os.ModeSymlink == 0 {
				return nil
			}
			target, e := hfLinkTarget(name)
			if e != nil {
				return e
			}
			if !pathWithin(blobRoot, target) || filepath.Dir(target) != blobRoot {
				return fmt.Errorf("snapshot link escapes repository blobs")
			}
			targetID, e := physicalEntryIdentity(target)
			if e != nil {
				return e
			}
			if refs[targetID] == nil {
				refs[targetID] = map[string]bool{}
			}
			refs[targetID][name] = true
			return nil
		})
		if err != nil {
			result.Errors = append(result.Errors, "could not completely inspect snapshots: "+err.Error())
			return result
		}
	}
	selectedInfos := make(map[string]os.FileInfo, len(selected))
	selectedTargets := make(map[string]string, len(selected))
	blobInfos := make(map[string]os.FileInfo, len(selected))
	selectedPathsByEntryID := make(map[string]string, len(selected))
	for name := range selected {
		info, e := os.Lstat(name)
		if e != nil || info.IsDir() || (!info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0) {
			result.Errors = append(result.Errors, "selected snapshot entry is missing or unsafe")
			return result
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := hfLinkTarget(name)
			targetInfo, targetErr := os.Lstat(target)
			if e != nil || !pathWithin(blobRoot, target) || targetErr != nil || !targetInfo.Mode().IsRegular() || targetInfo.Mode()&os.ModeSymlink != 0 {
				result.Errors = append(result.Errors, "selected snapshot link is unsafe")
				return result
			}
			selectedTargets[name] = target
			blobInfos[target] = targetInfo
		}
		selectedInfos[name] = info
		identity, e := physicalEntryIdentity(name)
		if e != nil {
			result.Errors = append(result.Errors, name+": selected entry identity could not be established")
			return result
		}
		selectedPathsByEntryID[identity] = name
	}
	candidateBlobs := make(map[string]string, len(selectedTargets))
	candidateBlobInfos := make(map[string]os.FileInfo, len(selectedTargets))
	for _, target := range selectedTargets {
		identity, e := physicalEntryIdentity(target)
		if e != nil {
			result.Errors = append(result.Errors, target+": payload identity could not be established")
			return result
		}
		candidateBlobs[identity] = target
		candidateBlobInfos[identity] = blobInfos[target]
	}
	snapshotFS, err := os.OpenRoot(snapshotRoot)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	defer snapshotFS.Close()
	blobFS, err := os.OpenRoot(blobRoot)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	defer blobFS.Close()
	var friendlyFS *os.Root
	if info, e := os.Lstat(friendlyRoot); e == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			result.Errors = append(result.Errors, "unsafe friendly view directory")
			return result
		}
		if e = safeDirectoryChain(friendlyRoot); e != nil {
			result.Errors = append(result.Errors, "unsafe friendly view path: "+e.Error())
			return result
		}
		friendlyFS, e = os.OpenRoot(friendlyRoot)
		if e != nil {
			result.Errors = append(result.Errors, e.Error())
			return result
		}
		defer friendlyFS.Close()
	} else if !os.IsNotExist(e) {
		result.Errors = append(result.Errors, e.Error())
		return result
	}
	// Friendly projections are removable only when their relative remote path
	// matches a selected snapshot entry. Other names are never authorized just
	// because they happen to point at selected data.
	friendly := []string{}
	friendlyLinks := map[string]string{}
	friendlyInfos := map[string]os.FileInfo{}
	friendlyEntryNames := map[string]string{}
	err = filepath.WalkDir(friendlyRoot, func(name string, d fs.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		info, e := os.Lstat(name)
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		target, e := hfLinkTarget(name)
		if e != nil {
			return e
		}
		friendlyLinks[name] = target
		friendlyInfos[name] = info
		identity, e := physicalEntryIdentity(name)
		if e != nil {
			return fmt.Errorf("friendly entry identity could not be established: %w", e)
		}
		if _, exists := friendlyEntryNames[identity]; !exists {
			friendlyEntryNames[identity] = name
		}
		return nil
	})
	if err != nil {
		result.Errors = append(result.Errors, "could not completely inspect friendly links: "+err.Error())
		return result
	}
	// Resolve bounded friendly-link chains so retained names cannot become
	// dangling dependencies and direct friendly->blob views retain shared data.
	var resolveFriendly func(string, map[string]bool) (string, error)
	resolveFriendly = func(name string, seen map[string]bool) (string, error) {
		if seen[name] {
			return "", fmt.Errorf("cyclic friendly link chain")
		}
		seen[name] = true
		target, ok := friendlyLinks[name]
		if !ok {
			return physicalEntryIdentity(name)
		}
		targetID, err := physicalEntryIdentity(target)
		if err != nil {
			return "", err
		}
		if _, selected := selectedPathsByEntryID[targetID]; selected {
			return targetID, nil
		}
		if next, ok := friendlyEntryNames[targetID]; ok {
			return resolveFriendly(next, seen)
		}
		if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 && !pathWithin(snapshotRoot, target) && !pathWithin(blobRoot, target) {
			return "", fmt.Errorf("friendly link chain leaves the inspected view")
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return targetID, nil
	}
	resolvedFriendlyTargets := make(map[string]string, len(friendlyLinks))
	for name := range friendlyLinks {
		target, e := resolveFriendly(name, map[string]bool{})
		if e != nil {
			result.Errors = append(result.Errors, name+": "+e.Error())
			return result
		}
		resolvedFriendlyTargets[name] = target
		if selectedPath, ok := selectedPathsByEntryID[target]; ok {
			rel, e := filepath.Rel(friendlyRoot, name)
			if e != nil {
				result.Errors = append(result.Errors, name+": friendly path could not be validated")
				return result
			}
			if filepath.ToSlash(rel) != selectedRemotePaths[selectedPath] {
				result.Errors = append(result.Errors, name+": retained friendly name depends on a selected snapshot entry")
				return result
			}
			for _, root := range cleanProtectedRoots {
				if pathWithin(root, name) {
					result.Errors = append(result.Errors, name+": selected friendly projection belongs to a configured local root")
					return result
				}
			}
			friendly = append(friendly, name)
		}
	}
	// Identify eligible payloads from the actual selected symlink targets only.
	retainedCandidates := map[string]bool{}
	removableFriendly := make(map[string]bool, len(friendly))
	for _, name := range friendly {
		removableFriendly[name] = true
	}
	for name, target := range resolvedFriendlyTargets {
		if _, candidate := candidateBlobs[target]; candidate {
			if !removableFriendly[name] {
				retainedCandidates[target] = true
			}
		}
	}
	result.Attempted = true
	retainedSelected := make(map[string]bool)
	for _, name := range friendly {
		rel, _ := filepath.Rel(friendlyRoot, name)
		if friendlyFS == nil {
			result.Errors = append(result.Errors, name+": friendly directory disappeared")
			result.Remaining = append(result.Remaining, name)
			if _, candidate := candidateBlobs[resolvedFriendlyTargets[name]]; candidate {
				retainedCandidates[resolvedFriendlyTargets[name]] = true
			}
			if selectedPath, ok := selectedPathsByEntryID[resolvedFriendlyTargets[name]]; ok {
				retainedSelected[selectedPath] = true
			}
			continue
		}
		current, statErr := friendlyFS.Lstat(rel)
		if statErr != nil || !os.SameFile(friendlyInfos[name], current) {
			result.Errors = append(result.Errors, name+": friendly entry changed after preflight")
			result.Remaining = append(result.Remaining, name)
			if _, candidate := candidateBlobs[resolvedFriendlyTargets[name]]; candidate {
				retainedCandidates[resolvedFriendlyTargets[name]] = true
			}
			if selectedPath, ok := selectedPathsByEntryID[resolvedFriendlyTargets[name]]; ok {
				retainedSelected[selectedPath] = true
			}
			continue
		}
		if e := friendlyFS.Remove(rel); e != nil {
			result.Errors = append(result.Errors, name+": "+e.Error())
			result.Remaining = append(result.Remaining, name)
			if _, candidate := candidateBlobs[resolvedFriendlyTargets[name]]; candidate {
				retainedCandidates[resolvedFriendlyTargets[name]] = true
			}
			if selectedPath, ok := selectedPathsByEntryID[resolvedFriendlyTargets[name]]; ok {
				retainedSelected[selectedPath] = true
			}
		} else {
			result.Removed = append(result.Removed, name)
		}
	}
	removedSelected := map[string]bool{}
	for name := range selected {
		if retainedSelected[name] {
			result.Remaining = append(result.Remaining, name)
			continue
		}
		rel, _ := filepath.Rel(snapshotRoot, name)
		current, statErr := snapshotFS.Lstat(rel)
		if statErr != nil || !os.SameFile(selectedInfos[name], current) {
			result.Errors = append(result.Errors, name+": selected entry changed after preflight")
			result.Remaining = append(result.Remaining, name)
			continue
		}
		if e := snapshotFS.Remove(rel); e != nil {
			result.Errors = append(result.Errors, name+": "+e.Error())
			result.Remaining = append(result.Remaining, name)
		} else {
			removedSelected[name] = true
			result.Removed = append(result.Removed, name)
		}
	}
	for blobID, blob := range candidateBlobs {
		if retainedCandidates[blobID] {
			result.RetainedBlobs = append(result.RetainedBlobs, blob)
			continue
		}
		stillReferenced := false
		for ref := range refs[blobID] {
			if !selected[ref] || !removedSelected[ref] {
				stillReferenced = true
				break
			}
		}
		if stillReferenced {
			result.RetainedBlobs = append(result.RetainedBlobs, blob)
			continue
		}
		info, e := os.Lstat(blob)
		if os.IsNotExist(e) {
			result.Errors = append(result.Errors, blob+": payload disappeared during deletion")
			result.Remaining = append(result.Remaining, blob)
			continue
		}
		if e != nil {
			result.Errors = append(result.Errors, blob+": "+e.Error())
			result.Remaining = append(result.Remaining, blob)
			continue
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			result.RetainedBlobs = append(result.RetainedBlobs, blob)
			continue
		}
		if prior, ok := candidateBlobInfos[blobID]; ok && !os.SameFile(prior, info) {
			result.Errors = append(result.Errors, blob+": payload changed after preflight")
			result.Remaining = append(result.Remaining, blob)
			continue
		}
		rel, relErr := filepath.Rel(blobRoot, blob)
		if relErr != nil || !cleanHFRelative(filepath.ToSlash(rel)) || strings.Contains(filepath.ToSlash(rel), "/") {
			result.RetainedBlobs = append(result.RetainedBlobs, blob)
			continue
		}
		if e = blobFS.Remove(rel); e != nil {
			result.Errors = append(result.Errors, blob+": "+e.Error())
			result.Remaining = append(result.Remaining, blob)
		} else {
			result.Removed = append(result.Removed, blob)
		}
	}
	sort.Strings(result.Removed)
	sort.Strings(result.Remaining)
	sort.Strings(result.RetainedBlobs)
	sort.Strings(result.Errors)
	return result
}
