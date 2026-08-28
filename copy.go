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

			for job := range prepared {
				if ctx.Err() != nil {
					job.finishSource()
					continue
				}
				if c.linearTargetStopped() {
					job.finishSource()
					continue
				}

				wrap(ctx, func() { c.write(ctx, job, ch, cntr, noSpaceDevices) })
			}
		})
	}

	return ch
}

func (c *Copyer) write(ctx context.Context, job *writeJob, ch chan<- *baseJob, cntr *counter, noSpaceDevices mapset.Set[string]) {
	// Release the source and publish ownership only after every consumer stops.
	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		job.finishSource()
		job.setStatus(jobStatusFinishing)
		select {
		case ch <- job.baseJob:
		case <-ctx.Done():
		}
	}()

	// Reject source changes before creating any targets.
	job.setStatus(jobStatusCopying)
	if job.size != job.stat.size {
		job.fail("", fmt.Errorf("source size changed, indexed=%d current=%d", job.stat.size, job.size))
		return
	}

	// Skip jobs only when every requested target device is already exhausted.
	targetDevices := lo.Map(job.targets, func(target string, _ int) string { return c.getDevice(target) })
	if len(targetDevices) > 0 && noSpaceDevices.Contains(targetDevices...) {
		job.fail("", ErrTargetNoSpace)
		return
	}

	// Track progress and close every consumer after the source reader finishes.
	atomic.AddInt64(&cntr.files, 1)
	chans := make([]chan []byte, 0, len(job.targets)+1)
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

		file, err := os.OpenFile(target, c.createFlag, job.stat.mode)
		if err = mappingError(err); err != nil {
			if checkErrorAbort(err) {
				noSpaceDevices.Add(dev)
			}
			c.endLinearTarget(err)

			job.fail(target, fmt.Errorf("open dst file fail, %w", err))
			continue
		}
		if !job.copyer.toDevice.linear && job.size > 0 {
			if err := truncate(file, job.size); err != nil {
				_ = file.Close()
				_ = os.Remove(target)
				job.fail(target, fmt.Errorf("truncate dst file fail, %w", err))
				continue
			}
		}

		// Consume source buffers in one managed writer for this target.
		ch := make(chan []byte, 4)
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
				for range ch {
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
			for buf := range ch {
				n, err := file.Write(buf)
				if err != nil {
					rerr = fmt.Errorf("write fail, %w", err)
					return
				}
				if len(buf) != n {
					rerr = fmt.Errorf("write fail, unexpected writen bytes return, read= %d write= %d", len(buf), n)
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
	if c.withHash {
		sha := sha256Pool.Get().(hash.Hash)
		sha.Reset()

		ch := make(chan []byte, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()
			defer sha256Pool.Put(sha)

			for buf := range ch {
				sha.Write(buf)
			}

			job.setHash(sha.Sum(nil))
		})
	}

	// Read the source only when at least one target or hash consumer needs it.
	if len(chans) == 0 {
		return
	}
	var copied int64
	copied, readErr = c.streamCopy(ctx, chans, job.reader, &cntr.bytes)
	if readErr == nil && copied != job.size {
		readErr = fmt.Errorf("source size changed while copying, expected=%d copied=%d", job.size, copied)
	}
	if readErr != nil && targetWriters == 0 {
		job.fail("", readErr)
	}
}

func (c *Copyer) streamCopy(ctx context.Context, dsts []chan []byte, src io.ReadCloser, bytes *int64) (int64, error) {
	var copied int64
	for {
		buf := make([]byte, batchSize)

		n, err := io.ReadFull(src, buf)
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				return copied, fmt.Errorf("slice mmap fail, %w", err)
			}
		}

		buf = buf[:n]
		for _, ch := range dsts {
			ch <- buf
		}

		nr := len(buf)
		copied += int64(nr)
		atomic.AddInt64(bytes, int64(nr))
		if nr < batchSize {
			return copied, nil
		}

		select {
		case <-ctx.Done():
			return copied, ctx.Err()
		default:
		}
	}
}
