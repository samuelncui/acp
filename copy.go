package acp

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	sha256 "github.com/minio/sha256-simd"
	"github.com/samber/lo"
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

func (c *Copyer) copy(ctx context.Context, prepared <-chan *writeJob) <-chan *baseJob {
	ch := make(chan *baseJob, 128)

	var copying sync.WaitGroup
	done := make(chan struct{})
	var reporting sync.WaitGroup
	defer func() {
		go wrap(ctx, func() {
			copying.Wait()
			close(done)
			reporting.Wait()
			close(ch)
		})
	}()

	cntr := new(counter)
	reporting.Add(1)
	go wrap(ctx, func() {
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
		go wrap(ctx, func() {
			defer copying.Done()

			// Consume prepared readers until the stage closes, so a stopped pipeline
			// still reports the items it holds instead of dropping them. One worker
			// writes one item at a time: that bounds target concurrency by the device
			// thread count and keeps a linear target in request order.
			for job := range prepared {
				if c.stopped(ctx) {
					job.finishSource()
					if !c.abandon(job.baseJob, c.abandonment(ctx)) {
						return
					}
					continue
				}

				c.write(ctx, job, ch, cntr, noSpaceDevices)
			}
		})
	}

	return ch
}

func (c *Copyer) write(ctx context.Context, job *writeJob, ch chan<- *baseJob, cntr *counter, noSpaceDevices mapset.Set[string]) {
	// Release the source and publish ownership only after every consumer stops. A
	// graceful stop still reports the item this stage finished.
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		job.finishSource()
		job.setStatus(jobStatusFinishing)
		select {
		case ch <- job.baseJob:
		case <-c.hardStop:
		}
	}()

	// Reject source changes before creating any targets. Every requested target is
	// reported failed, so the item stays a completion with failed outcomes.
	job.setStatus(jobStatusCopying)
	if job.skipContent {
		atomic.AddInt64(&cntr.files, 1)
		return
	}
	if job.size != job.stat.size {
		job.failAll(fmt.Errorf("source size changed, indexed=%d current=%d", job.stat.size, job.size))
		return
	}
	if job.cacheHit {
		atomic.AddInt64(&cntr.files, 1)
		atomic.AddInt64(&cntr.bytes, job.size)
		return
	}

	// Report every target failed when every requested target device is exhausted.
	targetDevices := lo.Map(job.targets, func(target string, _ int) string { return c.getDevice(target) })
	if len(targetDevices) > 0 && noSpaceDevices.Contains(targetDevices...) {
		job.failAll(ErrTargetNoSpace)
		return
	}

	// Track progress and close every consumer after the source reader finishes.
	atomic.AddInt64(&cntr.files, 1)
	chans := make([]chan *chunkBuffer, 0, len(job.targets)+1)
	defer func() {
		for _, ch := range chans {
			close(ch)
		}
	}()

	// Open each viable target before reading the source once.
	var readErr error
	for _, target := range job.targets {
		target := target

		// Reject exhausted targets before reserving capacity.
		dev := c.getDevice(target)
		if noSpaceDevices.Contains(dev) {
			job.fail(target, ErrTargetNoSpace)
			continue
		}

		if err := c.getDiskUsageCache(dev).check(job.size); err != nil {
			if errors.Is(err, ErrTargetNoSpace) {
				noSpaceDevices.Add(dev)
			}
			c.endLinearTarget(err)

			job.fail(target, fmt.Errorf("check disk usage have error, %w", err))
			continue
		}

		// Prepare the target file before attaching its stream consumer.
		if err := mappingError(os.MkdirAll(filepath.Dir(target), os.ModePerm)); err != nil {
			if checkErrorAbort(err) {
				noSpaceDevices.Add(dev)
			}
			c.endLinearTarget(err)

			job.fail(target, fmt.Errorf("mkdir dst dir fail, %w", err))
			continue
		}

		// Invalidate an existing cache before O_TRUNC can replace its content.
		if c.createFlag&os.O_TRUNC != 0 {
			c.invalidateSignaturePath(target)
		}
		file, err := os.OpenFile(target, c.createFlag, job.stat.mode)
		if err = mappingError(err); err != nil {
			if checkErrorAbort(err) {
				noSpaceDevices.Add(dev)
			}
			c.endLinearTarget(err)

			job.fail(target, fmt.Errorf("open dst file fail, %w", err))
			continue
		}
		c.invalidateSignature(file, target)
		if !job.copyer.toDevice.linear && job.size > 0 {
			if err := truncate(file, job.size); err != nil {
				_ = file.Close()
				_ = os.Remove(target)
				job.fail(target, fmt.Errorf("truncate dst file fail, %w", err))
				continue
			}
		}

		// Consume source buffers in one managed writer for this target.
		ch := make(chan *chunkBuffer, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()

			// Settle target status and discard any incomplete file before exiting.
			var rerr error
			defer func() {
				if rerr == nil {
					job.success(target)
					return
				}

				rerr = mappingError(rerr)
				if checkErrorAbort(rerr) {
					noSpaceDevices.Add(dev)
				}
				c.endLinearTarget(rerr)

				// avoid block channel
				for chunk := range ch {
					chunk.release()
				}

				job.fail(target, fmt.Errorf("write dst file fail, %w", rerr))
				if err := os.Remove(target); err != nil {
					c.reportError(job.path, target, fmt.Errorf("delete failed file has error, %w", err))
				}
			}()

			// Write every source buffer before publishing the durability boundary.
			defer func() {
				if file != nil {
					_ = file.Close()
				}
			}()
			for chunk := range ch {
				size := len(chunk.data)
				n, err := file.Write(chunk.data)
				chunk.release()
				if err != nil {
					rerr = fmt.Errorf("write fail, %w", err)
					return
				}
				if size != n {
					rerr = fmt.Errorf("write fail, unexpected writen bytes return, read= %d write= %d", size, n)
					return
				}
			}

			// A linear target publishes its durability boundary when the caller unmounts it.
			if !c.toDevice.linear {
				if err := file.Sync(); err != nil {
					rerr = fmt.Errorf("sync dst file fail, %w", err)
					return
				}
			}
			if err := file.Close(); err != nil {
				file = nil
				rerr = fmt.Errorf("close dst file fail, %w", err)
				return
			}
			file = nil
			if readErr != nil {
				rerr = readErr
				return
			}
		})
	}
	targetWriters := len(chans)

	// Add hashing as another consumer of the shared source stream.
	if c.hashPolicy.producesHash() {
		sha := sha256Pool.Get().(hash.Hash)
		sha.Reset()

		ch := make(chan *chunkBuffer, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()
			defer sha256Pool.Put(sha)

			for chunk := range ch {
				sha.Write(chunk.data)
				chunk.release()
			}

			// A stopped read describes only part of the source, so it has no content hash.
			if readErr != nil {
				job.setHash(nil)
				return
			}
			job.setHash(sha.Sum(nil))
		})
	}

	// Read the source only when at least one target or hash consumer needs it.
	if len(chans) == 0 {
		return
	}
	var copied int64
	copied, readErr = c.streamCopy(chans, job.reader, &cntr.bytes)
	if readErr == nil && copied != job.size {
		readErr = fmt.Errorf("source size changed while copying, expected=%d copied=%d", job.size, copied)
	}
	if readErr == nil && c.hashPolicy.producesHash() {
		job.validateHash()
	}
	if readErr != nil && targetWriters == 0 {
		// An item with no target outcome to report against could not be processed at all.
		c.reportItemError(job.path, "", readErr)
		job.itemError = readErr
	}
}

// streamCopy reads the source once and hands every chunk to each consumer. A graceful
// stop finishes the item in flight; only a pipeline failure ends the read early.
func (c *Copyer) streamCopy(dsts []chan *chunkBuffer, src io.ReadCloser, bytes *int64) (int64, error) {
	var copied int64
	for {
		chunk := acquireChunk()

		n, err := io.ReadFull(src, chunk.data)
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				chunk.release()
				return copied, fmt.Errorf("slice mmap fail, %w", err)
			}
		}

		// Every consumer releases its own reference; the producer releases the last one
		// after the size is captured, because the buffer may be reused immediately.
		chunk.data = chunk.data[:n]
		for _, ch := range dsts {
			ch <- chunk.retain()
		}
		copied += int64(n)
		atomic.AddInt64(bytes, int64(n))
		chunk.release()
		if n < batchSize {
			return copied, nil
		}

		select {
		case <-c.hardStop:
			return copied, fmt.Errorf("copy stopped by pipeline failure")
		default:
		}
	}
}
