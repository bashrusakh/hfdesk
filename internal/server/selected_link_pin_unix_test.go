// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOpenSelectedNonblockingRegularEntryDoesNotWaitForFIFOWriter(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "selected.gguf"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		f, openErr := openSelectedNonblockingRegularEntry(root, "selected.gguf")
		done <- result{file: f, err: openErr}
	}()

	select {
	case got := <-done:
		if got.file != nil {
			_ = got.file.Close()
		}
		if got.err != nil {
			t.Fatalf("open rooted FIFO nonblocking: %v", got.err)
		}
	case <-time.After(300 * time.Millisecond):
		writerDone := make(chan error, 1)
		go func() {
			writer, openErr := os.OpenFile(filepath.Join(dir, "selected.gguf"), os.O_WRONLY, 0)
			if openErr == nil {
				openErr = writer.Close()
			}
			writerDone <- openErr
		}()
		select {
		case got := <-done:
			if got.file != nil {
				_ = got.file.Close()
			}
			if got.err != nil {
				t.Errorf("release blocked FIFO open: %v", got.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("FIFO opener did not return after a writer was released")
		}
		select {
		case err := <-writerDone:
			if err != nil {
				t.Errorf("close FIFO writer: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("FIFO writer did not finish")
		}
		t.Fatal("rooted FIFO open blocked waiting for a writer")
	}
}

func TestOpenSelectedNonblockingRegularEntryPreservesRegularRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "selected.gguf"), []byte("regular content"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := openSelectedNonblockingRegularEntry(root, "selected.gguf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "regular content" {
		t.Fatalf("read regular entry = %q, %v", got, err)
	}
}
