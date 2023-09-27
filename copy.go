package acp

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
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

	ErrTargetNoSpace        = fmt.Errorf("acp: target have no space")
	ErrTargetDropToReadonly = fmt.Errorf("acp: target droped into readonly")
)

func (c *Copyer) copy(ctx context.Context, prepared <-chan *writeJob) <-chan *baseJob {
	ch := make(chan *baseJob, 128)

	var copying sync.WaitGroup
	done := make(chan struct{})
	defer func() {
		go wrap(ctx, func() {
			defer close(done)
			defer close(ch)

			copying.Wait()
		})
	}()

	cntr := new(counter)
	go wrap(ctx, func() {
		ticker := time.NewTicker(time.Second)
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

			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-prepared:
					if !ok {
						return
					}

					wrap(ctx, func() { c.write(ctx, job, ch, cntr, noSpaceDevices) })
				}
			}
		})
	}

	return ch
}

func (c *Copyer) write(ctx context.Context, job *writeJob, ch chan<- *baseJob, cntr *counter, noSpaceDevices mapset.Set[string]) {
	job.setStatus(jobStatusCopying)
	defer job.setStatus(jobStatusFinishing)

	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		job.done()
		ch <- job.baseJob
	}()

	// shortcut
	if noSpaceDevices.Contains(lo.Map(job.targets, func(target string, _ int) string { return c.getDevice(target) })...) {
		job.fail("", ErrTargetNoSpace)
		return
	}

	atomic.AddInt64(&cntr.files, 1)
	chans := make([]chan []byte, 0, len(job.targets)+1)
	defer func() {
		for _, ch := range chans {
			close(ch)
		}
	}()

	var readErr error
	for _, target := range job.targets {
		target := target

		dev := c.getDevice(target)
		if noSpaceDevices.Contains(dev) {
			job.fail(target, ErrTargetNoSpace)
			continue
		}

		if err := c.getDiskUsageCache(dev).check(job.size); err != nil {
			if errors.Is(err, ErrTargetNoSpace) {
				noSpaceDevices.Add(dev)
			}

			job.fail(target, fmt.Errorf("check disk usage have error, %w", err))
			continue
		}

		if err := os.MkdirAll(path.Dir(target), os.ModePerm); err != nil {
			// if no space
			if errors.Is(err, syscall.ENOSPC) {
				noSpaceDevices.Add(dev)
				job.fail(target, fmt.Errorf("%w, mkdir dst dir fail", ErrTargetNoSpace))
				continue
			}
			if errors.Is(err, syscall.EROFS) {
				noSpaceDevices.Add(dev)
				job.fail(target, fmt.Errorf("%w, mkdir dst dir fail", ErrTargetDropToReadonly))
				continue
			}

			job.fail(target, fmt.Errorf("mkdir dst dir fail, %w", err))
			continue
		}

		file, err := os.OpenFile(target, c.createFlag, job.mode)
		if err != nil {
			// if no space
			if errors.Is(err, syscall.ENOSPC) {
				noSpaceDevices.Add(dev)
				job.fail(target, fmt.Errorf("%w, open dst file fail", ErrTargetNoSpace))
				continue
			}
			if errors.Is(err, syscall.EROFS) {
				noSpaceDevices.Add(dev)
				job.fail(target, fmt.Errorf("%w, open dst file fail", ErrTargetDropToReadonly))
				continue
			}

			job.fail(target, fmt.Errorf("open dst file fail, %w", err))
			continue
		}

		ch := make(chan []byte, 4)
		chans = append(chans, ch)

		wg.Add(1)
		go wrap(ctx, func() {
			defer wg.Done()

			var rerr error
			defer func() {
				if rerr == nil {
					job.succes(target)
					return
				}

				// avoid block channel
				for range ch {
				}

				if err := os.Remove(target); err != nil {
					c.reportError(job.path, target, fmt.Errorf("delete failed file has error, %w", err))
				}

				// if no space
				if errors.Is(rerr, syscall.ENOSPC) {
					noSpaceDevices.Add(dev)
					job.fail(target, fmt.Errorf("%w, write dst file fail", ErrTargetNoSpace))
					return
				}
				if errors.Is(rerr, syscall.EROFS) {
					noSpaceDevices.Add(dev)
					job.fail(target, fmt.Errorf("%w, write dst file fail", ErrTargetDropToReadonly))
					return
				}

				job.fail(target, fmt.Errorf("write dst file fail, %w", rerr))
			}()

			defer file.Close()
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

			if readErr != nil {
				rerr = readErr
			}
		})
	}
	if len(chans) == 0 {
		return
	}

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
	readErr = c.streamCopy(ctx, chans, job.reader, &cntr.bytes)
}

func (c *Copyer) streamCopy(ctx context.Context, dsts []chan []byte, src io.ReadCloser, bytes *int64) error {
	for idx := int64(0); ; idx += batchSize {
		buf := make([]byte, batchSize)

		n, err := io.ReadFull(src, buf)
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				return fmt.Errorf("slice mmap fail, %w", err)
			}
		}

		buf = buf[:n]
		for _, ch := range dsts {
			ch <- buf
		}

		nr := len(buf)
		atomic.AddInt64(bytes, int64(nr))
		if nr < batchSize {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}
