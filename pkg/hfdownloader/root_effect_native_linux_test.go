//go:build linux

package hfdownloader

import (
	"bytes"
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
	cmd.Env = append(os.Environ(), nativeMountChildEnv+"=1")
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
		t.Fatalf("isolated native mount subprocess failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "native bind-mount cases exercised") {
		t.Fatalf("native mount subprocess did not confirm exercised fixtures:\n%s", output.String())
	}
}

func runNativeMountProtectedRootCases(t *testing.T) {
	required := os.Getenv("HFDESK_REQUIRE_NATIVE_MOUNTS") == "1"
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		if required {
			t.Fatalf("required private mount namespace unavailable: %v", err)
		}
		t.Skipf("cannot create private mount namespace: %v", err)
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
