// Test helper process for validating operating-system lease release on exit.
package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/bashrusakh/hfdesk/internal/runtimecoordination"
	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func main() {
	if base := os.Getenv("HFDESK_TEST_COORDINATION_DIR"); base != "" {
		runtimecoordination.SetBaseDirProvider(func() (string, error) { return base, nil })
	}
	if len(os.Args) == 3 && os.Args[1] == "shared" {
		sharedLease(os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "hold-path" {
		holdPathLease(os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "carrier-path" {
		carrierPath(os.Args[2])
		return
	}
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	release, err := hfdownloader.AcquireRebuildCoordinationLeases([]string{os.Args[1]}, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer release()
	fmt.Println("REBUILD_LEASE_HELD")
	select {}
}

func holdPathLease(root string) {
	path := hfdownloader.NewHFCache(root, 0).RebuildCoordinationPath()
	if path == "" {
		fmt.Fprintln(os.Stderr, "could not resolve data lease path")
		os.Exit(1)
	}
	fmt.Println("LEASE_PATH\t" + path)
	release, err := hfdownloader.AcquireRebuildCoordinationLeases([]string{root}, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer release()
	fmt.Println("LEASE_HELD")
	if _, err := bufio.NewReader(os.Stdin).ReadByte(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("LEASE_RELEASED")
}

// carrierPath prints the coordination carrier this account resolves for one
// root. It proves that two processes of the same account meet on one carrier
// even when the account has no name-service record.
func carrierPath(root string) {
	path := hfdownloader.NewHFCache(root, 0).RebuildCoordinationPath()
	if path == "" {
		fmt.Fprintln(os.Stderr, "could not resolve the coordination carrier")
		os.Exit(1)
	}
	fmt.Println("CARRIER_PATH\t" + path)
}

func sharedLease(root string) {
	input := bufio.NewReader(os.Stdin)
	fmt.Println("READY")
	if _, err := input.ReadByte(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	release, err := hfdownloader.AcquireRebuildCoordinationLeases([]string{root}, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("LEASE_HELD")
	if _, err := input.ReadByte(); err != nil {
		release()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	release()
}
