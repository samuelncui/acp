package acp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheConcurrentInitOnce(t *testing.T) {
	var initialized int64
	cached := Cache(func(in int) *int {
		atomic.AddInt64(&initialized, 1)
		out := new(int)
		*out = in
		return out
	})

	const workers = 64
	start := make(chan struct{})
	results := make(chan *int, workers)
	var wg sync.WaitGroup
	for idx := 0; idx < workers; idx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- cached(1)
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	if got := atomic.LoadInt64(&initialized); got != 1 {
		t.Fatalf("cache initialized %d times", got)
	}

	var first *int
	for result := range results {
		if first == nil {
			first = result
			continue
		}
		if result != first {
			t.Fatalf("cache returned different values")
		}
	}
}

func TestCacheInitializerCanLoadAnotherKey(t *testing.T) {
	var cached func(int) int
	cached = Cache(func(in int) int {
		if in == 0 {
			return 0
		}
		return cached(in-1) + 1
	})

	done := make(chan int, 1)
	go func() { done <- cached(2) }()

	select {
	case got := <-done:
		if got != 2 {
			t.Fatalf("cached value = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("cache initializer deadlocked")
	}
}
