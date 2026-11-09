// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"sync"
	"testing"
)

func TestConcurrentLoggingIsSafe(t *testing.T) {
	const (
		goroutines = 16
		perWorker  = 64
		capacity   = 100
	)

	p, _ := newTestPipeline(t, Options{RingCapacity: capacity})
	logger := p.Logger()

	var wg sync.WaitGroup
	for worker := range goroutines {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range perWorker {
				logger.Info("concurrent record", "worker", worker, "index", i)
			}
		}(worker)
	}

	// Read the ring while the writers are running.
	stop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.Records(0, RecentFirst)
			}
		}
	}()

	wg.Wait()
	close(stop)
	reader.Wait()

	records, _ := p.Records(0, OldestFirst)
	if len(records) != capacity {
		t.Errorf("got %d records, want %d", len(records), capacity)
	}
	if last := records[len(records)-1].Seq; last != goroutines*perWorker {
		t.Errorf("last sequence = %d, want %d", last, goroutines*perWorker)
	}
	for i := 1; i < len(records); i++ {
		if records[i].Seq != records[i-1].Seq+1 {
			t.Fatalf("sequence numbers are not monotonic: %d then %d", records[i-1].Seq, records[i].Seq)
		}
	}
}

func TestConcurrentSubscriptionLifecycleIsSafe(t *testing.T) {
	p, _ := newTestPipeline(t, Options{})
	logger := p.Logger()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 32 {
				sub := p.Subscribe()
				logger.Info("record", "source", "subscriber")
				sub.Close()
			}
		}()
	}
	wg.Wait()

	if records, _ := p.Records(0, OldestFirst); len(records) != 8*32 {
		t.Errorf("got %d records, want %d", len(records), 8*32)
	}
}
