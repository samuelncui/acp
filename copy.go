package acp

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	sha256 "github.com/minio/sha256-simd"
	"github.com/sirupsen/logrus"
)

const (
	batchSize = 1 * 1024 * 1024
)

var (
	sha256Pool = &sync.Pool{New: func() interface{} { return sha256.New() }}
)

// chunkBuffer is one read buffer shared by the hash consumer and every target writer of
// one item. Each consumer holds a reference and the last release returns the buffer to
// the shared pool, so allocation churn disappears while the read-ahead window stays
// exactly what the consumer channels allow.
type chunkBuffer struct {
	data []byte
	refs int32
}

var chunkPool = sync.Pool{
	New: func() interface{} {
		return &chunkBuffer{data: make([]byte, batchSize)}
	},
}

// acquireChunk returns a pooled buffer holding one producer reference.
func acquireChunk() *chunkBuffer {
	chunk := chunkPool.Get().(*chunkBuffer)
	chunk.data = chunk.data[:cap(chunk.data)]
	chunk.refs = 1
	return chunk
}

// retain adds one consumer reference to the buffer.
func (c *chunkBuffer) retain() *chunkBuffer {
	atomic.AddInt32(&c.refs, 1)
	return c
}

// release drops one reference and returns the buffer once no consumer holds it.
func (c *chunkBuffer) release() {
	if atomic.AddInt32(&c.refs, -1) == 0 {
		chunkPool.Put(c)
	}
}

func (c *StreamCopyer) copy(ctx context.Context, prepared <-chan *writeJob) <-chan *baseJob {
	// Every wait and handoff in this stage runs on a context that never cancels, so a stop
	// can never interrupt an item in flight.
	drained := context.WithoutCancel(ctx)

	ch := make(chan *baseJob, 128)

	var copying sync.WaitGroup
	done := make(chan struct{})
	var reporting sync.WaitGroup
	defer func() {
		go c.wrap(drained, func() {
			copying.Wait()
			close(done)
			reporting.Wait()
			close(ch)
		})
	}()

	cntr := new(counter)
	reporting.Add(1)
	go c.wrap(drained, func() {
		defer reporting.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.submit(&EventUpdateProgress{Bytes: atomic.LoadInt64(&cntr.bytes), Files: atomic.LoadInt64(&cntr.files)})
			case <-done:
				c.submit(&EventUpdateProgress{Bytes: atomic.LoadInt64(&cntr.bytes), Files: atomic.LoadInt64(&cntr.files), Finished: true})
				return
			}
		}
	})

	noSpaceDevices := mapset.NewSet[string]()
	for idx := 0; idx < c.toDevice.threads; idx++ {
		copying.Add(1)
		go c.wrap(drained, func() {
			defer copying.Done()

			// Consume prepared readers until the stage closes, so a stopped pipeline still
			// reports the items it holds instead of dropping them. One worker writes one
			// item at a time: that bounds target concurrency by the device thread count and
			// keeps a linear target in request order.
			for job := range prepared {
				// An item this stage cannot start is marked and forwarded: the reporting
				// stage fails it, which keeps exactly one terminal callback per item.
				if c.markUnstarted(ctx, job.baseJob) {
					job.finishSource()
					c.publish(ch, job.baseJob)
					continue
				}

				c.write(drained, job, ch, cntr, noSpaceDevices)
			}
		})
	}

	return ch
}

func (c *StreamCopyer) write(ctx context.Context, job *writeJob, ch chan<- *baseJob, cntr *counter, noSpaceDevices mapset.Set[string]) {
	// Register all lifetimes before starting consumers; a preparation panic must release their gates.
	var wg sync.WaitGroup
	var readErr error
	chans := make([]chan *chunkBuffer, 0, len(job.targets)+1)
	defer func() {
		if value := recover(); value != nil {
			readErr = panicError("copy item", value)
			job.itemError = readErr
			c.setError(readErr)
			c.stopHard()
		}
		for _, ch := range chans {
			close(ch)
		}
		wg.Wait()
		// Source release also runs if cache publication panics inside this finalization scope.
		func() {
			defer func() {
				if err := job.finishSource(); err != nil {
					job.itemError = errors.Join(job.itemError, err)
				}
			}()
			c.refreshCacheEntry(job.source, job.path, job.baseJob, false)
		}()
		c.publish(ch, job.baseJob)
	}()

	// Record the write time; source metadata remains the snapshot owned by indexing.
	job.startWrite()
	if job.skipContent {
		atomic.AddInt64(&cntr.files, 1)
		return
	}
	if job.cacheHit {
		atomic.AddInt64(&cntr.files, 1)
		atomic.AddInt64(&cntr.bytes, job.stat.size)
		return
	}

	// Track progress and close every consumer after the source reader finishes.
	atomic.AddInt64(&cntr.files, 1)

	// cacheGate releases the target writers once the item's content hash is complete, so a target
	// publishes its cache entry through its own descriptor before it finalises. It stays nil when
	// the policy produces no hash, because then no target has an entry to publish.
	var cacheGate chan struct{}
	if c.signatures != nil && c.hashPolicy.refreshesCache() {
		cacheGate = make(chan struct{})
	}

	// Add hashing as another consumer of the shared source stream.
	if c.hashPolicy.producesHash() {
		ch := make(chan *chunkBuffer, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go c.wrap(ctx, func() {
			defer wg.Done()
			defer func() {
				for chunk := range ch {
					chunk.release()
				}
			}()

			// The hash is complete, so the target writers may publish their cache entries. The
			// release is deferred: a panic in the hasher must not leave a writer blocked on the
			// gate, because Wait and Close would then never return after a recovered panic.
			if cacheGate != nil {
				defer close(cacheGate)
			}

			defer func() {
				if value := recover(); value != nil {
					err := panicError("hash consumer", value)
					c.setError(err)
					c.stopHard()
				}
			}()

			sha := sha256Pool.Get().(hash.Hash)
			defer sha256Pool.Put(sha)
			sha.Reset()
			for chunk := range ch {
				func() { defer chunk.release(); _, _ = sha.Write(chunk.data) }()
			}

			// A stopped read describes only part of the source, so it has no content hash.
			if readErr != nil {
				job.setHash(nil)
			} else {
				job.setHash(sha.Sum(nil))
			}
		})
	}

	// Open targets before reading once; each failed target leaves independent targets available.
	targetWriters := 0
	for _, target := range job.outputs {
		if noSpaceDevices.Contains(target.device) {
			job.fail(target.name, ErrTargetNoSpace)
			continue
		}
		out, err := c.prepareTarget(job, target)
		if err != nil {
			c.targetFailed(job.baseJob, target.name, target.device, err, noSpaceDevices)
			continue
		}
		func() {
			transferred := false
			defer func() {
				if !transferred {
					_ = out.Discard()
				}
			}()
			chunks := make(chan *chunkBuffer, 4)
			chans = append(chans, chunks)
			wg.Add(1)
			go c.wrap(ctx, func() {
				defer wg.Done()
				c.consumeTarget(job, target, out, chunks, cacheGate, &readErr, noSpaceDevices)
			})
			transferred = true
			targetWriters++
		}()
	}

	// Read the source only when at least one target or hash consumer needs it.
	if len(chans) == 0 {
		return
	}
	_, readErr = c.streamCopy(chans, job.reader, &cntr.bytes)
	if readErr == nil && c.hashPolicy.producesHash() {
		job.validateHash()
	}
	if readErr != nil && targetWriters == 0 {
		// An item with no target outcome to report against could not be processed at all.
		c.logf(logrus.ErrorLevel, "copy failed, source= %q, %v", job.path, readErr)
		job.itemError = readErr
	}
}

// streamCopy reads the source once and hands every chunk to each consumer. A graceful
// stop finishes the item in flight; only a pipeline failure ends the read early.
func (c *StreamCopyer) streamCopy(dsts []chan *chunkBuffer, src io.ReadCloser, bytes *int64) (int64, error) {
	// Each iteration owns one producer reference, with no deferred resources accumulating in the loop.
	var copied int64
	for {
		n, err := c.streamChunk(dsts, src)
		copied += int64(n)
		atomic.AddInt64(bytes, int64(n))
		if err != nil {
			return copied, err
		}
		if n < batchSize {
			return copied, nil
		}
	}
}

func (c *StreamCopyer) streamChunk(dsts []chan *chunkBuffer, src io.Reader) (int, error) {
	// Acquire and release in the same scope, including a panic in an injected reader.
	chunk := acquireChunk()
	defer chunk.release()
	n, err := io.ReadFull(src, chunk.data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read source failed, %w", err)
	}

	// Every successful handoff transfers exactly one reference to a consumer.
	chunk.data = chunk.data[:n]
	for _, ch := range dsts {
		receipt := chunk.retain()
		select {
		case ch <- receipt:
		case <-c.hardStop:
			receipt.release()
			return n, fmt.Errorf("copy stopped by pipeline failure")
		}
	}
	return n, nil
}
