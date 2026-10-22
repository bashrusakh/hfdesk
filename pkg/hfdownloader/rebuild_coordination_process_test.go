// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos || windows

package hfdownloader

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
)

var (
	coordinationHelperOnce sync.Once
	coordinationHelperPath string
	coordinationHelperErr  error
)

func buildRebuildLeaseHolder(t *testing.T) string {
	t.Helper()
	coordinationHelperOnce.Do(func() {
		moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			coordinationHelperErr = err
			return
		}
		coordinationHelperPath = filepath.Join(testHFDeskTempRoot, "rebuild-lock-holder")
		if runtime.GOOS == "windows" {
			coordinationHelperPath += ".exe"
		}
		build := exec.Command("go", "build", "-o", coordinationHelperPath, "./pkg/hfdownloader/testdata/rebuild_lock_holder.go")
		build.Dir = moduleRoot
		if output, err := build.CombinedOutput(); err != nil {
			coordinationHelperErr = fmt.Errorf("build lease holder process: %w: %s", err, output)
		}
	})
	if coordinationHelperErr != nil {
		t.Fatalf("build lease holder process: %v", coordinationHelperErr)
	}
	return coordinationHelperPath
}

func TestRebuildCoordinationLeaseProcessExitAndStableObject(t *testing.T) {
	root := t.TempDir()
	release, err := AcquireRebuildCoordinationLeases([]string{root}, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	lockPath := NewHFCache(root, 0).RebuildCoordinationPath()
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	command := exec.Command(buildRebuildLeaseHolder(t), root)
	command.Dir = t.TempDir()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	processFinished := false
	defer func() {
		if !processFinished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "REBUILD_LEASE_HELD" {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("helper did not confirm the acquired lease: %q", line)
		}
	case <-time.After(20 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("helper process did not acquire the rebuild lease")
	}
	held, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, held) {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("lease control object changed while held: before=%v after=%v err=%v", before, held, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	processFinished = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	releaseAfterExit, err := acquireRebuildCoordinationLeases(ctx, []string{root}, true, nil)
	if err != nil {
		t.Fatalf("process exit did not release its OS lease: %v", err)
	}
	releaseAfterExit()
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lease control object was not stable after process exit: before=%v after=%v err=%v", before, after, err)
	}
}

func TestRebuildCoordinationRefusesForeignCarrierRecord(t *testing.T) {
	root := t.TempDir()
	path := NewHFCache(root, 0).RebuildCoordinationPath()
	foreign := []byte("ordinary data is not a coordination lease record")
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := AcquireRebuildCoordinationLeases([]string{root}, true); err == nil {
		release()
		t.Fatal("coordination adopted a foreign regular file at its deterministic carrier path")
	}
	afterInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("foreign carrier entry was replaced: before=%v after=%v err=%v", beforeInfo, afterInfo, err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("foreign carrier contents changed: before=%q after=%q err=%v", beforeBytes, afterBytes, err)
	}
}

func TestRebuildCoordinationDoesNotAdoptUnownedDirectoryCandidate(t *testing.T) {
	root := t.TempDir()
	fixtureBase := filepath.Join(t.TempDir(), "coordination-fixture")
	restore := runtimecoordination.SetBaseDirProvider(func() (string, error) { return fixtureBase, nil })
	defer restore()
	base, err := privateCoordinationBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	foreignDir := filepath.Join(base, coordinationDirectoryName)
	if err := os.MkdirAll(foreignDir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignPath := filepath.Join(foreignDir, "foreign-lock-data")
	foreignBytes := []byte("unowned coordination candidate")
	if err := os.WriteFile(foreignPath, foreignBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRebuildCoordinationLeases([]string{root}, true); err == nil {
		t.Fatal("coordination adopted a pre-existing unmarked directory")
	}
	if _, err := os.Lstat(filepath.Join(foreignDir, coordinationOwnerFilename)); !os.IsNotExist(err) {
		t.Fatalf("foreign coordinator candidate received an owner marker: %v", err)
	}
	if got, err := os.ReadFile(foreignPath); err != nil || !bytes.Equal(got, foreignBytes) {
		t.Fatalf("foreign coordinator candidate changed: got=%q err=%v", got, err)
	}
}

func TestRebuildCoordinationTreePreflightProtectsOwnedCoordinator(t *testing.T) {
	restore := runtimecoordination.SetBaseDirProvider(nil)
	defer restore()
	root := t.TempDir()
	if NewHFCache(root, 0).RebuildCoordinationPath() == "" {
		t.Fatal("could not establish the canonical OS-profile coordinator")
	}
	scope, owned, err := ownedCoordinationDirectory()
	if err != nil || !owned {
		t.Fatalf("OS-profile coordinator owned=%t err=%v", owned, err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home, err := resolvedPathForCreate(current.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	if !coordinationPathsOverlap(home, scope) {
		t.Fatal("OS profile does not contain its coordinator scope")
	}
	if err := RejectBranchVariantTreeEffects(home); err == nil || !strings.Contains(err.Error(), "coordination scope") {
		t.Fatalf("recursive profile-tree preflight error=%v; want fail-before-traversal refusal", err)
	}
	if err := RejectBranchVariantTreeEffects(root); err != nil {
		t.Fatalf("unrelated temporary cache tree was rejected: %v", err)
	}
}

func TestRebuildCoordinationPathIgnoresPerProcessCacheOverride(t *testing.T) {
	restore := runtimecoordination.SetBaseDirProvider(nil)
	defer restore()
	root := t.TempDir()
	registerProfileCoordinationCleanup(t, root)
	before := NewHFCache(root, 0).RebuildCoordinationPath()
	if before == "" {
		t.Fatal("initial root coordination path is empty")
	}
	alternateHome := filepath.Join(t.TempDir(), "different-home")
	t.Setenv("HOME", alternateHome)
	t.Setenv("USERPROFILE", alternateHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(alternateHome, "alternate-xdg-cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(alternateHome, "alternate-xdg-config"))
	t.Setenv("LOCALAPPDATA", filepath.Join(alternateHome, "alternate-local-app-data"))
	t.Setenv("APPDATA", filepath.Join(alternateHome, "alternate-app-data"))
	t.Setenv("TMPDIR", filepath.Join(alternateHome, "alternate-tmpdir"))
	t.Setenv("TMP", filepath.Join(alternateHome, "alternate-tmp"))
	t.Setenv("TEMP", filepath.Join(alternateHome, "alternate-temp"))
	after := NewHFCache(root, 0).RebuildCoordinationPath()
	if after != before {
		t.Fatalf("same-user cache override split the physical-root coordinator: before=%q after=%q", before, after)
	}
}

func TestSameUIDDifferentEnvironmentChildSharesExclusiveRootLease(t *testing.T) {
	restore := runtimecoordination.SetBaseDirProvider(nil)
	defer restore()
	root := t.TempDir()
	registerProfileCoordinationCleanup(t, root)
	parentRoot := root
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := createCoordinationDirectoryAliasForTest(alias, root); err == nil {
		parentRoot = alias
	} else {
		t.Logf("physical-root alias unavailable for direct lease probe: %v", err)
	}
	parentPath := NewHFCache(parentRoot, 0).RebuildCoordinationPath()
	if parentPath == "" {
		t.Fatal("parent physical-root lease path is empty")
	}
	command := exec.Command(buildRebuildLeaseHolder(t), "hold-path", root)
	command.Dir = t.TempDir()
	alternate := filepath.Join(t.TempDir(), "other-runtime-context")
	command.Env = rebuildTestEnvWithOverrides(map[string]string{
		"HOME": alternate, "USERPROFILE": alternate,
		"XDG_CACHE_HOME": filepath.Join(alternate, "cache"), "XDG_CONFIG_HOME": filepath.Join(alternate, "config"),
		"LOCALAPPDATA": filepath.Join(alternate, "localappdata"), "APPDATA": filepath.Join(alternate, "appdata"),
		"TMPDIR": filepath.Join(alternate, "tmpdir"), "TMP": filepath.Join(alternate, "tmp"), "TEMP": filepath.Join(alternate, "temp"),
		"HFDESK_TEST_COORDINATION_DIR": "",
	})
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	lines := make(chan string, 3)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	pathLine := receiveCoordinationHelperLine(t, lines, "child lease path")
	const pathPrefix = "LEASE_PATH\t"
	if !strings.HasPrefix(pathLine, pathPrefix) {
		t.Fatalf("child first line=%q; want lease path", pathLine)
	}
	childPath := strings.TrimPrefix(pathLine, pathPrefix)
	if !sameBranchVariantPath(parentPath, childPath) {
		t.Fatalf("same OS user/root split coordinator identity across environment overrides: parent=%q child=%q", parentPath, childPath)
	}
	if line := receiveCoordinationHelperLine(t, lines, "child exclusive lease"); line != "LEASE_HELD" {
		t.Fatalf("child lease line=%q; want LEASE_HELD", line)
	}

	waiting := make(chan struct{}, 1)
	previousHook := rebuildCoordinationContentionHook
	rebuildCoordinationContentionHook = func(path string, exclusive bool) {
		if exclusive && sameBranchVariantPath(path, parentPath) {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
	}
	defer func() { rebuildCoordinationContentionHook = previousHook }()
	parentDone := make(chan struct {
		release func()
		err     error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		release, err := acquireRebuildCoordinationLeases(ctx, []string{parentRoot}, true, nil)
		parentDone <- struct {
			release func()
			err     error
		}{release: release, err: err}
	}()
	select {
	case <-waiting:
	case result := <-parentDone:
		if result.release != nil {
			result.release()
		}
		t.Fatalf("parent lease completed while different-environment child held exclusive lease: %v", result.err)
	case <-ctx.Done():
		t.Fatal("parent did not contend on the child-held physical-root lease")
	}
	if _, err := stdin.Write([]byte{'R'}); err != nil {
		t.Fatal(err)
	}
	if line := receiveCoordinationHelperLine(t, lines, "child lease release"); line != "LEASE_RELEASED" {
		t.Fatalf("child release line=%q; want LEASE_RELEASED", line)
	}
	result := <-parentDone
	if result.err != nil {
		t.Fatalf("parent exclusive lease after child release: %v", result.err)
	}
	result.release()
	_ = stdin.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("different-environment lease helper: %v", err)
	}
	finished = true
}

func receiveCoordinationHelperLine(t *testing.T, lines <-chan string, purpose string) string {
	t.Helper()
	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatalf("helper exited before reporting %s", purpose)
		}
		return line
	case <-time.After(20 * time.Second):
		t.Fatalf("helper did not report %s", purpose)
		return ""
	}
}

func TestRebuildCoordinationConcurrentBootstrapAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	helper := buildRebuildLeaseHolder(t)
	commands := make([]*exec.Cmd, 2)
	inputs := make([]io.WriteCloser, 2)
	lines := make([]chan string, 2)
	finished := make([]bool, 2)
	for i := range commands {
		command := exec.Command(helper, "shared", root)
		stdin, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		command.Stderr = &bytes.Buffer{}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = command
		inputs[i] = stdin
		lines[i] = make(chan string, 2)
		go func(ch chan<- string, r io.Reader) {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				ch <- scanner.Text()
			}
			close(ch)
		}(lines[i], stdout)
	}
	defer func() {
		for i, command := range commands {
			if !finished[i] && command != nil && command.Process != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		}
	}()
	for i, ch := range lines {
		select {
		case line := <-ch:
			if line != "READY" {
				t.Fatalf("bootstrap helper %d ready line=%q", i, line)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("bootstrap helper %d did not reach the start barrier", i)
		}
	}
	for _, stdin := range inputs {
		if _, err := stdin.Write([]byte{'S'}); err != nil {
			t.Fatal(err)
		}
	}
	for i, ch := range lines {
		select {
		case line := <-ch:
			if line != "LEASE_HELD" {
				t.Fatalf("bootstrap helper %d lease line=%q", i, line)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("bootstrap helper %d did not acquire the shared root lease", i)
		}
	}
	coordinationPath := NewHFCache(root, 0).RebuildCoordinationPath()
	ownerBefore, err := os.Stat(coordinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if PathInside(root, coordinationPath) {
		t.Fatalf("coordination lease file occupied the selected cache root: %s", coordinationPath)
	}
	if _, err := os.Lstat(filepath.Join(root, BranchVariantServiceDirName)); !os.IsNotExist(err) {
		t.Fatalf("ordinary root admission claimed the EXL service tree: %v", err)
	}
	for i, stdin := range inputs {
		if _, err := stdin.Write([]byte{'R'}); err != nil {
			t.Fatal(err)
		}
		_ = stdin.Close()
		if err := commands[i].Wait(); err != nil {
			t.Fatalf("bootstrap helper %d: %v", i, err)
		}
		finished[i] = true
	}
	ownerAfter, err := os.Stat(coordinationPath)
	if err != nil || !os.SameFile(ownerBefore, ownerAfter) {
		t.Fatalf("concurrent bootstrap replaced the shared root lease object: before=%v after=%v err=%v", ownerBefore, ownerAfter, err)
	}
}

func TestFirstBranchClaimWaitsForCrossProcessOrdinaryAdmission(t *testing.T) {
	root := t.TempDir()
	command := exec.Command(buildRebuildLeaseHolder(t), "shared", root)
	command.Dir = t.TempDir()
	alternateHome := filepath.Join(t.TempDir(), "different-home")
	command.Env = rebuildTestEnvWithOverrides(map[string]string{
		"HOME":           alternateHome,
		"USERPROFILE":    alternateHome,
		"XDG_CACHE_HOME": filepath.Join(alternateHome, "cache"),
		"LOCALAPPDATA":   filepath.Join(alternateHome, "localappdata"),
	})
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = &bytes.Buffer{}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	lines := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	for _, want := range []string{"READY"} {
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("admission helper line=%q; want %q", got, want)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("cross-process admission helper did not reach its start barrier")
		}
	}
	if _, err := stdin.Write([]byte{'S'}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-lines:
		if got != "LEASE_HELD" {
			t.Fatalf("cross-process admission helper line=%q; want LEASE_HELD", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cross-process helper did not acquire shared root/admission leases")
	}

	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{
		"4.00bpw": {commit: "branch-commit", files: map[string][]byte{"weights.safetensors": []byte("branch-payload")}},
	})
	defer branchHub.Close()
	admissionPath, err := rebuildCoordinationAdmissionFilePath(root)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{}, 1)
	rebuildCoordinationContentionHook = func(path string, exclusive bool) {
		if exclusive && path == admissionPath {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
	}
	defer func() { rebuildCoordinationContentionHook = nil }()
	branchDone := make(chan error, 1)
	go func() {
		branchDone <- Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, Settings{
			OutputDir: root, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256",
		}, nil)
	}()
	select {
	case <-waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("first branch claim did not wait on the other process's admission lease")
	}
	serviceRoot, err := BranchVariantServiceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(serviceRoot); !os.IsNotExist(err) {
		t.Fatalf("branch claimed service root before the foreign admission lease was released: %v", err)
	}
	if _, err := stdin.Write([]byte{'R'}); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("cross-process shared admission helper: %v", err)
	}
	finished = true
	if err := <-branchDone; err != nil {
		t.Fatalf("branch claim after other process released admission: %v", err)
	}
	if owned, err := BranchVariantServiceOwned(root); err != nil || !owned {
		t.Fatalf("first branch claim owned=%t err=%v", owned, err)
	}
}

func TestSyncCleanKeepsStableRootLeaseObject(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := NewHFCache(root, 0)
	if _, err := cache.Sync(SyncOptions{Clean: true}); err != nil {
		t.Fatal(err)
	}
	lockPath := cache.RebuildCoordinationPath()
	if PathInside(root, lockPath) {
		t.Fatalf("Sync carrier was placed in the model/cache tree: %s", lockPath)
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("Sync(Clean:true) removed the root coordination object: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("root coordination object has unsafe type: %v", info.Mode())
	}
	if _, err := os.Lstat(filepath.Join(root, BranchVariantServiceDirName)); !os.IsNotExist(err) {
		t.Fatalf("ordinary Sync claimed the EXL service tree for coordination: %v", err)
	}
}

func TestSyncCleanRefusesToRemoveOwnedCoordinationScope(t *testing.T) {
	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	if err := os.MkdirAll(cache.HubDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// Place the coordinator inside the friendly view a recursive clean walks,
	// so the clean would reach the app-owned coordination entries.
	coordinationBase := filepath.Join(cache.ModelsDir(), "coordination-base")
	restore := runtimecoordination.SetBaseDirProvider(func() (string, error) { return coordinationBase, nil })
	defer restore()
	scope, err := ensurePrivateCoordinationDirectory(context.Background())
	if err != nil {
		t.Fatalf("materialize the owned coordination scope: %v", err)
	}
	ownerMarker := filepath.Join(scope, coordinationOwnerFilename)
	ownerBefore, err := os.ReadFile(ownerMarker)
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(cache.ModelsDir(), "ordinary", "dangling-link")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	orphanCreated := os.Symlink(filepath.Join(cacheRoot, "missing-target"), orphan) == nil

	if _, err := cache.Sync(SyncOptions{Clean: true}); err == nil || !strings.Contains(err.Error(), "coordination scope") {
		t.Fatalf("clean of a scope containing the app-owned coordinator error=%v; want fail-before-effect refusal", err)
	}
	if _, err := os.Lstat(ownerMarker); err != nil {
		t.Fatalf("refused clean removed the coordination owner marker: %v", err)
	}
	ownerAfter, err := os.ReadFile(ownerMarker)
	if err != nil || !bytes.Equal(ownerBefore, ownerAfter) {
		t.Fatalf("refused clean changed the coordination owner marker: %q err=%v", ownerAfter, err)
	}
	if orphanCreated {
		if _, err := os.Lstat(orphan); err != nil {
			t.Fatalf("refused clean still performed ordinary cleanup: %v", err)
		}
	}
}

func TestSyncCleanLeavesProfileCoordinatorOutsideCacheScope(t *testing.T) {
	cacheRoot := t.TempDir()
	cache := NewHFCache(cacheRoot, 0)
	if err := os.MkdirAll(cache.HubDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := cache.RebuildCoordinationPath()
	if lockPath == "" || coordinationPathsOverlap(cache.ModelsDir(), lockPath) || coordinationPathsOverlap(cache.DatasetsDir(), lockPath) {
		t.Fatalf("OS-profile coordinator unexpectedly overlaps cache clean targets: %q", lockPath)
	}
	createLease, err := AcquireRebuildCoordinationLeases([]string{cacheRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	createLease()
	lockBefore, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lockBytesBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryOrphan := filepath.Join(cache.ModelsDir(), "ordinary", "dangling-link")
	if err := os.MkdirAll(filepath.Dir(ordinaryOrphan), 0o755); err != nil {
		t.Fatal(err)
	}
	orphanCreated := os.Symlink(filepath.Join(cacheRoot, "missing-target"), ordinaryOrphan) == nil
	if _, err := cache.Sync(SyncOptions{Clean: true}); err != nil {
		t.Fatalf("clean of selected cache root entered the OS-profile coordinator: %v", err)
	}
	if orphanCreated {
		if _, err := os.Lstat(ordinaryOrphan); !os.IsNotExist(err) {
			t.Fatalf("cache clean did not remove its ordinary orphan: %v", err)
		}
	}
	lockAfter, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatalf("cache clean replaced the profile coordinator carrier: before=%v after=%v err=%v", lockBefore, lockAfter, err)
	}
	lockBytesAfter, err := os.ReadFile(lockPath)
	if err != nil || !bytes.Equal(lockBytesBefore, lockBytesAfter) {
		t.Fatalf("cache clean changed the profile coordinator record: before=%q after=%q err=%v", lockBytesBefore, lockBytesAfter, err)
	}
}

func TestOrdinaryDownloadCannotReplaceOwnedCoordinationCarrier(t *testing.T) {
	leaseRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(leaseRoot, "hub"), 0o755); err != nil {
		t.Fatal(err)
	}
	releaseLease, err := AcquireRebuildCoordinationLeases([]string{leaseRoot}, true)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			releaseLease()
		}
	}()
	carrierPath := NewHFCache(leaseRoot, 0).RebuildCoordinationPath()
	carrierBefore, err := os.Stat(carrierPath)
	if err != nil {
		t.Fatal(err)
	}
	carrierBytesBefore, err := os.ReadFile(carrierPath)
	if err != nil {
		t.Fatal(err)
	}
	coordBase, err := privateCoordinationBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	tempRoot := t.TempDir()
	ownerAlias := filepath.Join(tempRoot, "owner")
	if err := createCoordinationDirectoryAliasForTest(ownerAlias, coordBase); err != nil {
		t.Skipf("directory alias unavailable for exact-carrier mutation probe: %v", err)
	}
	defer os.Remove(ownerAlias)
	repoID := "owner/coordination"
	relativeCarrier := filepath.Base(carrierPath)
	ordinaryTarget := filepath.Join(tempRoot, filepath.FromSlash(repoID), relativeCarrier)
	resolvedOrdinaryTarget, err := resolvedPathForCreate(ordinaryTarget)
	if err != nil || !sameBranchVariantPath(resolvedOrdinaryTarget, carrierPath) {
		t.Fatalf("ordinary accepted local path %q does not resolve to the actual carrier %q", ordinaryTarget, carrierPath)
	}
	var payloadRequested atomic.Bool
	hub := newBranchVariantTestHubForRepo(t, repoID, map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{relativeCarrier: []byte("ordinary carrier payload")}},
	}, func(_, _ string) { payloadRequested.Store(true) })
	defer hub.Close()
	err = Download(context.Background(), Job{Repo: repoID, Revision: "main"}, Settings{
		OutputDir: tempRoot, Endpoint: hub.URL, Concurrency: 1, Verify: "size",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "coordination scope") {
		t.Fatalf("ordinary payload at the active coordinator path error=%v; want pre-effect refusal", err)
	}
	if payloadRequested.Load() {
		t.Fatal("ordinary payload transfer started before the coordinator collision was refused")
	}
	carrierAfter, err := os.Stat(carrierPath)
	if err != nil || !os.SameFile(carrierBefore, carrierAfter) {
		t.Fatalf("ordinary download replaced the active coordinator carrier: before=%v after=%v err=%v", carrierBefore, carrierAfter, err)
	}
	carrierBytesAfter, err := os.ReadFile(carrierPath)
	if err != nil || !bytes.Equal(carrierBytesBefore, carrierBytesAfter) {
		t.Fatalf("ordinary download changed coordinator ownership data: before=%q after=%q err=%v", carrierBytesBefore, carrierBytesAfter, err)
	}

	// The same repository and filename remain ordinary data when the physical
	// target is outside the validated coordinator scope.
	ordinaryOutput := t.TempDir()
	payloadRequested.Store(false)
	if err := Download(context.Background(), Job{Repo: repoID, Revision: "main"}, Settings{
		OutputDir: ordinaryOutput, Endpoint: hub.URL, Concurrency: 1, Verify: "size",
	}, nil); err != nil {
		t.Fatalf("same-spelled ordinary path outside the coordinator scope was rejected: %v", err)
	}
	if !payloadRequested.Load() {
		t.Fatal("same-spelled ordinary path outside the coordinator scope did not transfer")
	}
	ordinaryFile := filepath.Join(ordinaryOutput, filepath.FromSlash(repoID), relativeCarrier)
	if got, err := os.ReadFile(ordinaryFile); err != nil || string(got) != "ordinary carrier payload" {
		t.Fatalf("same-spelled ordinary output=%q err=%v", got, err)
	}

	waiting := make(chan struct{}, 1)
	rebuildCoordinationContentionHook = func(path string, exclusive bool) {
		if exclusive && path == carrierPath {
			select {
			case waiting <- struct{}{}:
			default:
			}
		}
	}
	defer func() { rebuildCoordinationContentionHook = nil }()
	syncDone := make(chan error, 1)
	go func() {
		_, err := NewHFCache(leaseRoot, 0).Sync(SyncOptions{})
		syncDone <- err
	}()
	select {
	case <-waiting:
	case err := <-syncDone:
		t.Fatalf("Sync completed while the original exclusive carrier was held (err=%v)", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Sync did not contend on the original exclusive coordinator carrier")
	}
	select {
	case err := <-syncDone:
		t.Fatalf("Sync crossed the held exclusive carrier after the collision attempt: %v", err)
	default:
	}
	releaseLease()
	held = false
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("Sync after releasing the original coordinator carrier: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Sync did not complete after the original coordinator carrier was released")
	}
	carrierFinal, err := os.Stat(carrierPath)
	if err != nil || !os.SameFile(carrierBefore, carrierFinal) {
		t.Fatalf("coordinator carrier lifetime changed across Sync: before=%v after=%v err=%v", carrierBefore, carrierFinal, err)
	}
}

func TestOrdinaryHFWriterPreservesNestedCoordinationCarrier(t *testing.T) {
	root := t.TempDir()
	nestedRoot := filepath.Join(root, "models", "owner", "model")
	if err := os.MkdirAll(nestedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireRebuildCoordinationLeases([]string{nestedRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	carrierPath := NewHFCache(nestedRoot, 0).RebuildCoordinationPath()
	carrierBefore, err := os.Lstat(carrierPath)
	if err != nil {
		t.Fatal(err)
	}

	fileBytes := []byte("ordinary model payload")
	hub := newBranchVariantTestHubForRepo(t, "owner/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{".hfdesk-rebuild.lock": fileBytes}},
	}, nil)
	defer hub.Close()
	if err := Download(context.Background(), Job{Repo: "owner/model"}, Settings{
		CacheDir: root, Endpoint: hub.URL, Concurrency: 1, Verify: "size",
	}, nil); err != nil {
		t.Fatalf("ordinary download of the accepted lock-name path: %v", err)
	}

	carrierAfter, err := os.Lstat(carrierPath)
	if err != nil || !os.SameFile(carrierBefore, carrierAfter) {
		t.Fatalf("ordinary download replaced the nested coordination carrier: before=%v after=%v err=%v", carrierBefore, carrierAfter, err)
	}
	got, err := os.ReadFile(filepath.Join(nestedRoot, ".hfdesk-rebuild.lock"))
	if err != nil || !bytes.Equal(got, fileBytes) {
		t.Fatalf("ordinary payload at the accepted non-control path=%q err=%v", got, err)
	}
}

func TestOrdinaryLocalWriterUsesUnownedServiceOwnerSpelling(t *testing.T) {
	root := t.TempDir()
	nestedRoot := filepath.Join(root, "owner", "model")
	release, err := AcquireRebuildCoordinationLeases([]string{nestedRoot}, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	ownerPath := NewHFCache(nestedRoot, 0).RebuildCoordinationPath()
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerInfo, err := os.Stat(ownerPath)
	if err != nil {
		t.Fatal(err)
	}

	var payloadRequest atomic.Bool
	hub := newBranchVariantTestHubForRepo(t, "owner/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{
			BranchVariantServiceDirName + "/" + branchVariantStoreOwnerFilename: []byte("ordinary same-spelled payload"),
		}},
	}, func(_, _ string) { payloadRequest.Store(true) })
	defer hub.Close()
	settings := Settings{OutputDir: root, RebuildCoordinationRoots: []string{root}, Endpoint: hub.URL, Concurrency: 1, Verify: "size"}
	err = Download(context.Background(), Job{Repo: "owner/model"}, settings, nil)
	if err != nil {
		t.Fatalf("ordinary writer was blocked by an unowned service-name path: %v", err)
	}
	if !payloadRequest.Load() {
		t.Fatal("ordinary file at the unowned service-name path did not transfer")
	}
	carrierAfter, err := os.Stat(ownerPath)
	if err != nil || !os.SameFile(ownerInfo, carrierAfter) {
		t.Fatalf("ordinary writer changed the private coordination carrier: before=%v after=%v err=%v", ownerInfo, carrierAfter, err)
	}
	ordinaryPath := filepath.Join(nestedRoot, BranchVariantServiceDirName, branchVariantStoreOwnerFilename)
	if got, err := os.ReadFile(ordinaryPath); err != nil || string(got) != "ordinary same-spelled payload" {
		t.Fatalf("ordinary same-spelled payload=%q err=%v", got, err)
	}
	if after, err := os.ReadFile(ownerPath); err != nil || !bytes.Equal(ownerBefore, after) {
		t.Fatalf("ordinary writer changed the private coordination carrier: %q err=%v", after, err)
	}
}

func TestOrdinaryWriteWinsBeforeFirstBranchServiceClaim(t *testing.T) {
	root := t.TempDir()
	branchRoot := filepath.Join(root, "owner", "model")
	serviceRoot, err := BranchVariantServiceRoot(branchRoot)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryBytes := []byte("ordinary-first-data")
	ordinaryHub := newBranchVariantTestHubForRepo(t, "owner/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{
			filepath.ToSlash(filepath.Join(BranchVariantServiceDirName, "config.json")): ordinaryBytes,
		}},
	}, nil)
	defer ordinaryHub.Close()
	branchHub := newBranchVariantTestHubForRepo(t, "branch/model", map[string]branchVariantTestRevision{
		"4.00bpw": {commit: "branch-commit", files: map[string][]byte{"weights.safetensors": []byte("branch-bytes")}},
	}, nil)
	defer branchHub.Close()

	ordinaryReady := make(chan struct{})
	continueOrdinary := make(chan struct{})
	var releaseOnce sync.Once
	releaseOrdinary := func() { releaseOnce.Do(func() { close(continueOrdinary) }) }
	defer releaseOrdinary()
	ordinarySettings := Settings{OutputDir: root, RebuildCoordinationRoots: []string{root}, Endpoint: ordinaryHub.URL, Concurrency: 1, Verify: "size"}
	ordinarySettings.OnPlanComplete = func(Plan) {
		close(ordinaryReady)
		<-continueOrdinary
	}
	ordinaryDone := make(chan error, 1)
	go func() {
		ordinaryDone <- Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, ordinarySettings, nil)
	}()
	select {
	case <-ordinaryReady:
	case <-time.After(20 * time.Second):
		t.Fatal("ordinary app write did not reach its post-admission/pre-effect barrier")
	}

	claimWaiting := make(chan struct{}, 1)
	rebuildCoordinationContentionHook = func(_ string, exclusive bool) {
		if exclusive {
			select {
			case claimWaiting <- struct{}{}:
			default:
			}
		}
	}
	defer func() { rebuildCoordinationContentionHook = nil }()
	branchSettings := Settings{OutputDir: branchRoot, RebuildCoordinationRoots: []string{root}, Endpoint: branchHub.URL, Concurrency: 1, Verify: "size"}
	branchDone := make(chan error, 1)
	go func() {
		branchDone <- Download(context.Background(), Job{Repo: "branch/model", Revision: "4.00bpw", BranchVariant: true}, branchSettings, nil)
	}()
	select {
	case <-claimWaiting:
	case <-time.After(20 * time.Second):
		releaseOrdinary()
		<-ordinaryDone
		<-branchDone
		t.Fatal("first branch claim did not contend on the ordinary writer's root lease")
	}
	ordinaryPath := filepath.Join(serviceRoot, "config.json")
	if _, err := os.Lstat(serviceRoot); !os.IsNotExist(err) {
		t.Fatalf("branch service root was claimed while ordinary writer held admission: %v", err)
	}
	if _, err := os.Lstat(ordinaryPath); !os.IsNotExist(err) {
		t.Fatalf("ordinary payload was written before its effect barrier released: %v", err)
	}

	releaseOrdinary()
	if err := <-ordinaryDone; err != nil {
		t.Fatalf("ordinary writer that won admission: %v", err)
	}
	if got, err := os.ReadFile(ordinaryPath); err != nil || !bytes.Equal(got, ordinaryBytes) {
		t.Fatalf("ordinary-first bytes=%q err=%v", got, err)
	}
	branchErr := <-branchDone
	if branchErr == nil || !strings.Contains(branchErr.Error(), "unowned branch variant service tree") {
		t.Fatalf("later EXL claim error=%v; want fail-before-change on the ordinary-owned candidate", branchErr)
	}
	if got, err := os.ReadFile(ordinaryPath); err != nil || !bytes.Equal(got, ordinaryBytes) {
		t.Fatalf("refused EXL claim changed ordinary-first data: %q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)); !os.IsNotExist(err) {
		t.Fatalf("refused EXL claim installed an owner marker over ordinary data: %v", err)
	}
}

func TestFirstBranchServiceClaimWinsBeforeOrdinaryWriter(t *testing.T) {
	root := t.TempDir()
	branchRoot := filepath.Join(root, "owner", "model")
	serviceRoot, err := BranchVariantServiceRoot(branchRoot)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryPath := filepath.Join(serviceRoot, "config.json")
	ordinaryBytes := []byte("ordinary-must-not-replace-service-data")
	ordinaryHub := newBranchVariantTestHubForRepo(t, "owner/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{
			filepath.ToSlash(filepath.Join(BranchVariantServiceDirName, "config.json")): ordinaryBytes,
		}},
	}, nil)
	defer ordinaryHub.Close()
	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{
		"4.00bpw": {commit: "branch-commit", files: map[string][]byte{"weights.safetensors": []byte("branch-payload")}},
	})
	defer branchHub.Close()
	rootIdentity, err := rebuildCoordinationIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	claimReady := make(chan struct{})
	releaseClaim := make(chan struct{})
	var claimOnce sync.Once
	letClaimProceed := func() { claimOnce.Do(func() { close(releaseClaim) }) }
	defer letClaimProceed()
	var readyOnce sync.Once
	rebuildCoordinationAdmissionLeaseAcquiredHook = func(identity string, exclusive bool) {
		if exclusive && identity == rootIdentity {
			readyOnce.Do(func() { close(claimReady) })
			<-releaseClaim
		}
	}
	defer func() { rebuildCoordinationAdmissionLeaseAcquiredHook = nil }()
	branchSettings := Settings{OutputDir: branchRoot, RebuildCoordinationRoots: []string{root}, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256"}
	branchDone := make(chan error, 1)
	go func() {
		branchDone <- Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, branchSettings, nil)
	}()
	select {
	case <-claimReady:
	case <-time.After(20 * time.Second):
		t.Fatal("branch claim did not acquire exclusive root admission")
	}
	if _, err := os.Lstat(serviceRoot); !os.IsNotExist(err) {
		t.Fatalf("test barrier did not precede the first service-root claim: %v", err)
	}

	ordinaryWaiting := make(chan struct{}, 1)
	rootLeasePath, err := rebuildCoordinationAdmissionFilePath(root)
	if err != nil {
		t.Fatal(err)
	}
	rebuildCoordinationContentionHook = func(path string, exclusive bool) {
		if !exclusive && path == rootLeasePath {
			select {
			case ordinaryWaiting <- struct{}{}:
			default:
			}
		}
	}
	ordinarySettings := Settings{OutputDir: root, RebuildCoordinationRoots: []string{root}, Endpoint: ordinaryHub.URL, Concurrency: 1, Verify: "size"}
	ordinaryDone := make(chan error, 1)
	go func() {
		ordinaryDone <- Download(context.Background(), Job{Repo: "owner/model", Revision: "main"}, ordinarySettings, nil)
	}()
	select {
	case <-ordinaryWaiting:
	case <-time.After(20 * time.Second):
		letClaimProceed()
		<-branchDone
		<-ordinaryDone
		t.Fatal("ordinary writer did not contend on the branch claim's root lease")
	}
	if _, err := os.Lstat(ordinaryPath); !os.IsNotExist(err) {
		t.Fatalf("ordinary payload was written before branch ownership transition completed: %v", err)
	}
	letClaimProceed()
	if err := <-branchDone; err != nil {
		t.Fatalf("first branch claim: %v", err)
	}
	ordinaryErr := <-ordinaryDone
	if ordinaryErr == nil || !strings.Contains(ordinaryErr.Error(), "app-owned EXL branch variant service tree") {
		t.Fatalf("later ordinary writer error=%v; want refusal after service claim", ordinaryErr)
	}
	if _, err := os.Lstat(ordinaryPath); !os.IsNotExist(err) {
		t.Fatalf("ordinary writer created a file inside the claimed service root: %v", err)
	}
	artifact, err := ReadBranchVariantArtifact(branchRoot, RepoTypeModel, "owner/model", "4.00bpw", "branch-commit")
	if err != nil || artifact == nil {
		t.Fatalf("branch artifact after refused ordinary write=%+v err=%v", artifact, err)
	}
	if got, err := os.ReadFile(filepath.Join(artifact.PayloadRoot, "weights.safetensors")); err != nil || string(got) != "branch-payload" {
		t.Fatalf("branch payload after refused ordinary write=%q err=%v", got, err)
	}
}

func TestOrdinaryHFDownloadIgnoresUnownedRootServiceCandidateAndClaimRefuses(t *testing.T) {
	root := t.TempDir()
	serviceRoot, err := BranchVariantServiceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(serviceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(serviceRoot, "foreign-data")
	if err := os.WriteFile(foreign, []byte("keep-unowned"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignOwner := filepath.Join(serviceRoot, branchVariantStoreOwnerFilename)
	foreignOwnerBytes := []byte("not an HFDesk ownership record")
	if err := os.WriteFile(foreignOwner, foreignOwnerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryHub := newBranchVariantTestHubForRepo(t, "other/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{"config.json": []byte("unrelated ordinary file")}},
	}, nil)
	defer ordinaryHub.Close()
	branchHub := newBranchVariantTestHub(t, map[string]branchVariantTestRevision{
		"4.00bpw": branchVariantTestRevisions()["4.00bpw"],
	})
	defer branchHub.Close()
	if err := Download(context.Background(), Job{Repo: "other/model", Revision: "main"}, Settings{
		CacheDir: root, Endpoint: ordinaryHub.URL, Concurrency: 1, Verify: "size",
	}, nil); err != nil {
		t.Fatalf("unrelated ordinary HF download was blocked by an unowned service candidate: %v", err)
	}
	ordinaryPath := filepath.Join(root, "models", "other", "model", "config.json")
	if got, err := os.ReadFile(ordinaryPath); err != nil || string(got) != "unrelated ordinary file" {
		t.Fatalf("ordinary download output=%q err=%v", got, err)
	}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, Settings{
		CacheDir: root, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err == nil || !strings.Contains(err.Error(), "unowned branch variant service tree") {
		t.Fatalf("EXL download claim error=%v; want fail-before-change on the unowned service tree", err)
	}
	if got, readErr := os.ReadFile(foreign); readErr != nil || !bytes.Equal(got, before) {
		t.Fatalf("ordinary download/EXL claim changed foreign service-root content: %q err=%v", got, readErr)
	}
	if got, readErr := os.ReadFile(foreignOwner); readErr != nil || !bytes.Equal(got, foreignOwnerBytes) {
		t.Fatalf("ordinary download/EXL claim changed foreign owner-marker candidate: %q err=%v", got, readErr)
	}

	fileRoot := t.TempDir()
	fileServiceRoot, err := BranchVariantServiceRoot(fileRoot)
	if err != nil {
		t.Fatal(err)
	}
	foreignFile := []byte("foreign service-root file")
	if err := os.WriteFile(fileServiceRoot, foreignFile, 0o600); err != nil {
		t.Fatal(err)
	}
	fileHub := newBranchVariantTestHubForRepo(t, "other/model", map[string]branchVariantTestRevision{
		"main": {commit: "ordinary-commit", files: map[string][]byte{"config.json": []byte("ordinary")}},
	}, nil)
	defer fileHub.Close()
	if err := Download(context.Background(), Job{Repo: "other/model", Revision: "main"}, Settings{
		CacheDir: fileRoot, Endpoint: fileHub.URL, Concurrency: 1, Verify: "size",
	}, nil); err != nil {
		t.Fatalf("ordinary HF download was blocked by a regular-file service candidate: %v", err)
	}
	if err := Download(context.Background(), Job{Repo: "owner/model", Revision: "4.00bpw", BranchVariant: true}, Settings{
		CacheDir: fileRoot, Endpoint: branchHub.URL, Concurrency: 1, Verify: "sha256",
	}, nil); err == nil || !strings.Contains(err.Error(), "unowned branch variant service path") {
		t.Fatalf("EXL download claim error=%v; want refusal of a regular-file service path", err)
	}
	if got, err := os.ReadFile(fileServiceRoot); err != nil || !bytes.Equal(got, foreignFile) {
		t.Fatalf("ordinary download/EXL claim changed foreign service-root file: %q err=%v", got, err)
	}
}

func rebuildTestEnvWithOverrides(overrides map[string]string) []string {
	return overrideRebuildTestEnv(os.Environ(), overrides)
}

func overrideRebuildTestEnv(base []string, overrides map[string]string) []string {
	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func TestRebuildCoordinationLeavesLegacyRootSidecarUnclaimed(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			legacyPath := filepath.Join(root, ".hfdesk-rebuild.lock")
			if kind == "file" {
				if err := os.WriteFile(legacyPath, []byte("foreign legacy data"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(legacyPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(legacyPath, "marker"), []byte("foreign legacy data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			release, err := AcquireRebuildCoordinationLeases([]string{root}, false)
			if err != nil {
				t.Fatal(err)
			}
			release()
			checkPath := legacyPath
			if kind == "directory" {
				checkPath = filepath.Join(legacyPath, "marker")
			}
			if got, err := os.ReadFile(checkPath); err != nil || string(got) != "foreign legacy data" {
				t.Fatalf("legacy root %s was adopted or changed: %q err=%v", kind, got, err)
			}
		})
	}
}
