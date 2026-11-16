// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader_test

import (
	"context"
	"fmt"
	"os"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

func ExampleDownload() {
	job := hfdownloader.Job{
		Repo:     "hf-internal-testing/tiny-random-gpt2",
		Revision: "main",
	}

	dir, cleanup := exampleOutputDir()
	defer cleanup()

	cfg := hfdownloader.Settings{
		OutputDir:          dir,
		Concurrency:        4,
		MaxActiveDownloads: 2,
		Endpoint:           exampleEndpoint(), // a local stub so the example runs offline
	}

	// Progress callback
	progress := func(e hfdownloader.ProgressEvent) {
		switch e.Event {
		case "scan_start":
			fmt.Println("Scanning repository...")
		case "file_done":
			fmt.Printf("Downloaded: %s\n", e.Path)
		case "done":
			fmt.Println("Complete!")
		}
	}

	ctx := context.Background()
	err := hfdownloader.Download(ctx, job, cfg, progress)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}

	// Output:
	// Scanning repository...
	// Downloaded: config.json
	// Complete!
}

func ExampleDownload_withFilters() {
	// Download only specific quantizations
	job := hfdownloader.Job{
		Repo:    "TheBloke/Mistral-7B-Instruct-v0.2-GGUF",
		Filters: []string{"q4_k_m", "q5_k_m"}, // Case-insensitive matching
	}

	dir, cleanup := exampleOutputDir()
	defer cleanup()

	cfg := hfdownloader.Settings{
		OutputDir: dir,
		Endpoint:  exampleEndpoint(),
	}

	err := hfdownloader.Download(context.Background(), job, cfg, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	for _, p := range exampleRelativeFiles(dir) {
		fmt.Println(p)
	}

	// Output:
	// TheBloke/Mistral-7B-Instruct-v0.2-GGUF/mistral-7b-instruct-v0.2.Q4_K_M.gguf
	// TheBloke/Mistral-7B-Instruct-v0.2-GGUF/mistral-7b-instruct-v0.2.Q5_K_M.gguf
}

func ExampleDownload_dataset() {
	// Download a dataset instead of a model
	job := hfdownloader.Job{
		Repo:      "facebook/flores",
		IsDataset: true,
	}

	dir, cleanup := exampleOutputDir()
	defer cleanup()

	cfg := hfdownloader.Settings{
		OutputDir: dir,
		Endpoint:  exampleEndpoint(),
	}

	err := hfdownloader.Download(context.Background(), job, cfg, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	for _, p := range exampleRelativeFiles(dir) {
		fmt.Println(p)
	}

	// Output:
	// facebook/flores/dataset_infos.json
	// facebook/flores/dev.dev
}

func ExamplePlanRepo() {
	job := hfdownloader.Job{
		Repo: "hf-internal-testing/tiny-random-gpt2",
	}

	cfg := hfdownloader.Settings{
		Endpoint: exampleEndpoint(),
	}

	plan, err := hfdownloader.PlanRepo(context.Background(), job, cfg)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Printf("Found %d files:\n", len(plan.Items))
	for _, item := range plan.Items {
		lfsTag := ""
		if item.LFS {
			lfsTag = " [LFS]"
		}
		fmt.Printf("  %s (%d bytes)%s\n", item.RelativePath, item.Size, lfsTag)
	}

	// Output:
	// Found 1 files:
	//   config.json (647 bytes)
}

func ExampleIsValidModelName() {
	// Valid names
	fmt.Println(hfdownloader.IsValidModelName("TheBloke/Mistral-7B-Instruct-v0.2-GGUF")) // true
	fmt.Println(hfdownloader.IsValidModelName("facebook/opt-1.3b"))                      // true
	fmt.Println(hfdownloader.IsValidModelName("hf-internal-testing/tiny-gpt"))           // true

	// Invalid names
	fmt.Println(hfdownloader.IsValidModelName("Mistral-7B-GGUF")) // false (no owner)
	fmt.Println(hfdownloader.IsValidModelName(""))                // false (empty)
	fmt.Println(hfdownloader.IsValidModelName("/model"))          // false (empty owner)

	// Output:
	// true
	// true
	// true
	// false
	// false
	// false
}

func ExampleJob_filterSubdirs() {
	// Organize downloaded files by filter match
	job := hfdownloader.Job{
		Repo:               "TheBloke/Llama-2-7B-GGUF",
		Filters:            []string{"q4_0", "q5_0"},
		AppendFilterSubdir: true, // Creates separate subdirectories
	}

	// This will create (under the output directory):
	// TheBloke/Llama-2-7B-GGUF/q4_0/llama-2-7b.Q4_0.gguf
	// TheBloke/Llama-2-7B-GGUF/q5_0/llama-2-7b.Q5_0.gguf

	dir, cleanup := exampleOutputDir()
	defer cleanup()

	cfg := hfdownloader.Settings{
		OutputDir: dir,
		Endpoint:  exampleEndpoint(),
	}

	err := hfdownloader.Download(context.Background(), job, cfg, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	for _, p := range exampleRelativeFiles(dir) {
		fmt.Println(p)
	}

	// Output:
	// TheBloke/Llama-2-7B-GGUF/q4_0/llama-2-7b.Q4_0.gguf
	// TheBloke/Llama-2-7B-GGUF/q5_0/llama-2-7b.Q5_0.gguf
}

func ExampleSettings_withAuth() {
	// For private or gated repositories
	dir, cleanup := exampleOutputDir()
	defer cleanup()

	cfg := hfdownloader.Settings{
		OutputDir: dir,
		Token:     os.Getenv("HF_TOKEN"), // Use environment variable
		Endpoint:  exampleEndpoint(),
	}

	job := hfdownloader.Job{
		Repo: "meta-llama/Llama-2-7b", // Requires authentication
	}

	err := hfdownloader.Download(context.Background(), job, cfg, nil)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	for _, p := range exampleRelativeFiles(dir) {
		fmt.Println(p)
	}

	// Output:
	// meta-llama/Llama-2-7b/pytorch_model.bin
}

func ExampleSettings_performance() {
	// High-performance settings for fast networks; pass cfg to
	// Download() or PlanRepo().
	cfg := hfdownloader.Settings{
		OutputDir:          "./Models",
		Concurrency:        16,       // 16 parallel connections per file
		MaxActiveDownloads: 4,        // 4 files at once
		MultipartThreshold: "16MiB",  // Use multipart for files >= 16MiB
		Retries:            6,        // More retries for unstable connections
		BackoffInitial:     "200ms",  // Faster retry
		BackoffMax:         "30s",    // Longer max for rate limiting
		Verify:             "sha256", // Full verification
	}

	fmt.Printf("OutputDir: %s\n", cfg.OutputDir)
	fmt.Printf("Concurrency: %d\n", cfg.Concurrency)
	fmt.Printf("MaxActiveDownloads: %d\n", cfg.MaxActiveDownloads)
	fmt.Printf("MultipartThreshold: %s\n", cfg.MultipartThreshold)
	fmt.Printf("Retries: %d\n", cfg.Retries)
	fmt.Printf("BackoffInitial: %s\n", cfg.BackoffInitial)
	fmt.Printf("BackoffMax: %s\n", cfg.BackoffMax)
	fmt.Printf("Verify: %s\n", cfg.Verify)

	// Output:
	// OutputDir: ./Models
	// Concurrency: 16
	// MaxActiveDownloads: 4
	// MultipartThreshold: 16MiB
	// Retries: 6
	// BackoffInitial: 200ms
	// BackoffMax: 30s
	// Verify: sha256
}
