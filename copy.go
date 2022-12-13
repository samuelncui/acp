package acp

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/abc950309/acp/mmap"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/hashicorp/go-multierror"
	sha256 "github.com/minio/sha256-simd"
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

	badDsts := mapset.NewSet[string]()
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
					if badDsts.Cardinality() >= len(c.dst) {
						return
					}

					wrap(ctx, func() { c.write(ctx, job, ch, cntr, badDsts) })
				}
			}
		})
	}

	return ch
}

func (c *Copyer) write(ctx context.Context, job *writeJob, ch chan<- *baseJob, cntr *counter, badDsts mapset.Set[string]) {
	job.setStatus(jobStatusCopying)
	defer job.setStatus(jobStatusFinishing)

	var wg sync.WaitGroup
	defer func() {
		wg.Wait()
		job.done()
		ch <- job.baseJob
	}()

	atomic.AddInt64(&cntr.files, 1)
	chans := make([]chan []byte, 0, len(c.dst)+1)
	defer func() {
		for _, ch := range chans {
			close(ch)
		}
	}()

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

	var readErr error
	for _, d := range c.dst {
		dst := d
		name := job.source.dst(dst)

		if badDsts.Contains(dst) {
			job.fail(name, fmt.Errorf("bad target path"))
			continue
		}
		if err := os.MkdirAll(path.Dir(name), os.ModePerm); err != nil {
			job.fail(name, fmt.Errorf("mkdir dst dir fail, %w", err))
			continue
		}

		file, err := os.OpenFile(name, c.createFlag, job.mode)
		if err != nil {
			job.fail(name, fmt.Errorf("open dst file fail, %w", err))
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
					job.succes(name)
					return
				}

				// avoid block channel
				for range ch {
				}

				if re := os.Remove(name); re != nil {
					rerr = multierror.Append(rerr, re)
				}

				// if no space
				if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EROFS) {
					badDsts.Add(dst)
				}

				c.reportError(job.source.src(), name, rerr)
				job.fail(name, rerr)
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
	readErr = c.streamCopy(ctx, chans, job.src, &cntr.bytes)
}

func (c *Copyer) streamCopy(ctx context.Context, dsts []chan []byte, src *mmap.ReaderAt, bytes *int64) error {
	if src.Len() == 0 {
		return nil
	}

	for idx := int64(0); ; idx += batchSize {
		buf, err := src.Slice(idx, batchSize)
		if err != nil {
			return fmt.Errorf("slice mmap fail, %w", err)
		}

		copyed := make([]byte, len(buf))
		copy(copyed, buf)
		for _, ch := range dsts {
			ch <- copyed
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
