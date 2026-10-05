//go:build linux

package hfdownloader

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const nativeMountChildEnv = "HFDESK_NATIVE_MOUNT_CHILD"

// Run fixtures only in a private child mount namespace, never in the caller's
// namespace. Required CI mode makes every setup limitation a test failure.
func TestNativeMountProtectedRootReachability(t *testing.T) {
	if os.Getenv(nativeMountChildEnv) == "1" {
		runNativeMountProtectedRootCases(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestNativeMountProtectedRootReachability$")
	cmd.Env = nativeMountTestEnv(true)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if strings.Contains(output.String(), "--- SKIP:") {
		if os.Getenv("HFDESK_REQUIRE_NATIVE_MOUNTS") == "1" {
			t.Fatalf("required native mount fixture skipped:\n%s", output.String())
		}
		t.Skipf("native mount fixture unavailable:\n%s", output.String())
	}
	if err != nil {
		if os.Getenv("HFDESK_REQUIRE_NATIVE_MOUNTS") != "1" && !strings.Contains(output.String(), "--- FAIL:") {
			t.Skipf("cannot start private mount-namespace process: %v", err)
		}
		t.Fatalf("isolated native mount subprocess failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "native bind-mount cases exercised") {
		t.Fatalf("native mount subprocess did not confirm exercised fixtures:\n%s", output.String())
	}
}

func TestNativeMountWorkerMarkerCannotBypassNamespaceIsolation(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestNativeMountProtectedRootReachability$")
	cmd.Env = nativeMountTestEnv(true)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err == nil {
		t.Fatalf("worker marker entered native fixture without namespace isolation:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "native mount fixture is not isolated in a distinct process namespace") {
		t.Fatalf("worker marker was not rejected by the namespace check: %v\n%s", err, output.String())
	}
	if strings.Contains(output.String(), "cannot make fixture mount namespace private") || strings.Contains(output.String(), "bind-mount") {
		t.Fatalf("unsafe worker reached mount setup before isolation rejection:\n%s", output.String())
	}
}

func nativeMountTestEnv(worker bool) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, nativeMountChildEnv+"=") {
			continue
		}
		env = append(env, value)
	}
	if worker {
		env = append(env, nativeMountChildEnv+"=1")
	}
	return env
}

func runNativeMountProtectedRootCases(t *testing.T) {
	required := os.Getenv("HFDESK_REQUIRE_NATIVE_MOUNTS") == "1"
	currentNamespace, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatalf("identify worker mount namespace: %v", err)
	}
	parentNamespace, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", os.Getppid()))
	if err != nil || currentNamespace == parentNamespace {
		t.Fatalf("native mount fixture is not isolated in a distinct process namespace: worker=%q kernel-parent=%q err=%v", currentNamespace, parentNamespace, err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		if required {
			t.Fatalf("cannot make fixture mount namespace private: %v", err)
		}
		t.Skipf("cannot make fixture mount namespace private: %v", err)
	}
	base, err := os.MkdirTemp("", "hfdesk-native-bind-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	for _, friendly := range []bool{false, true} {
		for _, dataset := range []bool{false, true} {
			name := "model"
			if dataset {
				name = "dataset"
			}
			if friendly {
				name = "friendly-" + name
			}
			t.Run(name, func(t *testing.T) {
				hub := filepath.Join(base, "cache", "hub")
				repoName, projectionName, friendlyName, projectionRole := "models--owner--model", "models", "model", ManagedRootModelProjection
				if dataset {
					repoName, projectionName, friendlyName, projectionRole = "datasets--owner--dataset", "datasets", "dataset", ManagedRootDatasetProjection
				}
				target := filepath.Join(hub, repoName)
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				cache := filepath.Join(base, "cache")
				friendlyPath := filepath.Join(cache, projectionName, "owner", friendlyName)
				if err := os.MkdirAll(friendlyPath, 0o755); err != nil {
					t.Fatal(err)
				}
				effect := target
				if friendly {
					effect = friendlyPath
				}
				mounted := filepath.Join(effect, "application-data")
				if err := os.Mkdir(mounted, 0o755); err != nil {
					t.Fatal(err)
				}
				source := filepath.Join(base, "protected")
				if err := os.Mkdir(source, 0o755); err != nil {
					t.Fatal(err)
				}
				sentinel := filepath.Join(source, "keep.txt")
				if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mount(source, mounted, "", syscall.MS_BIND, ""); err != nil {
					if required {
						t.Fatalf("required bind mount unavailable: %v", err)
					}
					t.Skipf("cannot bind-mount disposable fixture: %v", err)
				}
				defer func() {
					if err := syscall.Unmount(mounted, syscall.MNT_DETACH); err != nil {
						t.Errorf("unmount disposable fixture %q: %v", mounted, err)
					}
				}()
				set := NewManagedRootSet(base, []ManagedRootSpec{
					{Path: hub, Roles: ManagedRootHub | ManagedRootProtected},
					{Path: filepath.Join(cache, projectionName), Roles: ManagedRootProtected | projectionRole},
					{Path: source, Roles: ManagedRootProtected},
				})
				if friendly {
					err = set.LegacyHFDeleteAllowed(ManagedRootID(hub, base), target, friendlyPath, cache)
				} else {
					err = set.WholeCopyAllowed(ManagedRootID(hub, base), target)
				}
				if err == nil {
					t.Fatal("actual bind-mounted protected object was accepted for recursive removal")
				}
				if _, err := os.Stat(sentinel); err != nil {
					t.Fatalf("protected sentinel was not preserved: %v", err)
				}
			})
		}
	}
	t.Log("native bind-mount cases exercised")
}
