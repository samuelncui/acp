package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/samuelncui/acp/mmap"
	"github.com/sirupsen/logrus"
)

type prepareResult struct {
	order   uint64
	job     *writeJob
	release chan struct{}
}

// itemSource is the one descriptor an item owns for its whole lifecycle, together with the reader
// that delivers its content. The reader is what closes the descriptor: an ordinary file is the
// reader itself, and a mapping is released before its descriptor closes.
type itemSource struct {
	file   *os.File
	reader io.ReadCloser
	size   int64
}

// openSourceContent opens the single descriptor an item owns, together with the reader that
// delivers its content. It is a variable so a test can observe which descriptor an item's cache
// read and cache write use.
var openSourceContent = func(path string, mode ReadMode) (itemSource, error) {
	// Only a caller that asks for it reads through a mapping, which retains its descriptor.
	if mode == ReadMapped {
		readerAt, err := mmap.Open(path)
		if err != nil {
			return itemSource{}, fmt.Errorf("open src file by mmap fail, %w", err)
		}

		return itemSource{
			file:   readerAt.File(),
			reader: mmap.NewReader(readerAt),
			size:   int64(readerAt.Len()),
		}, nil
	}

	// Every other source is read buffered, without updating its access time.
	file, err := openSource(path)
	if err != nil {
		return itemSource{}, fmt.Errorf("open src file fail, %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return itemSource{}, fmt.Errorf("get src file stat fail, %w", err)
	}
	return itemSource{file: file, reader: file, size: info.Size()}, nil
}

// prepareItem opens the one descriptor an item owns and reads its stored hash through it. That
// descriptor serves the whole item: the stored hash is read through it, the content is read
// through it, and the computed hash is published through it before it closes.
func (c *StreamCopyer) prepareItem(job *baseJob) *writeJob {
	// An item needs its source when it writes somewhere or hashes content, and a targetless item
	// reads its stored hash only where the policy reuses one.
	needsContent := len(job.targets) > 0 || c.hashPolicy.readsContent()
	reuses := c.signatures != nil && len(job.targets) == 0 && c.hashPolicy.reusesCache()

	// A policy that neither reads content nor uses the cache completes without a source.
	if !needsContent && !reuses {
		return c.noContentJob(job)
	}

	// Only an item that reads content opens a mapping: a reuse-only item reads a stored hash.
	mode := job.readMode
	if !needsContent {
		mode = ReadBuffered
	}
	source, err := openSourceContent(job.path, mode)
	if err != nil {
		// A reuse-only item owns its descriptor for the stored hash alone, so a source it cannot
		// open is a cache miss with a warning instead of an item failure.
		if reuses {
			c.signatures.recordFailure(job.path, err)
			c.signatures.incrementMiss()
			if !needsContent {
				return c.noContentJob(job)
			}
		}

		// A source that cannot be opened is an item outcome. The marked job travels to the
		// reporting stage instead of a side channel.
		c.logf(logrus.ErrorLevel, "prepare source failed, source= %q, %v", job.path, err)
		job.itemError = err
		return newWriteJob(job, nil, 0, false)
	}

	// Enter preparation through the descriptor the item already owns: a stored hash is reused only
	// where the policy allows it.
	reused := false
	if reuses {
		if hash, ok := c.signatures.lookup(source.file, job.path, job.stat); ok {
			job.setCachedHash(hash)
			source.size = job.stat.size
			reused = true
		}
	}

	wj := newWriteJob(job, source.reader, source.size, c.fromDevice.linear)
	wj.source = source.file
	if !needsContent && !reused {
		// An item that reused no stored hash has no hash at all and is not a cache hit. Its
		// descriptor still closes with the item, which owns it for its whole lifecycle.
		job.setHash(nil)
		wj.skipContent = true
	}
	return wj
}

// noContentJob settles an item whose policy needs no source content: the copy stage reports it
// without reading anything, and it is neither a cache hit nor a hash this run computed.
func (c *StreamCopyer) noContentJob(job *baseJob) *writeJob {
	job.setHash(nil)
	wj := newWriteJob(job, nil, 0, c.fromDevice.linear)
	wj.skipContent = true
	return wj
}

// prepare opens the sources of indexed jobs. It never acts on a stop itself: a job the stop
// reached before its source opened is marked and forwarded, so the reporting stage fails it
// instead of dropping it.
func (c *StreamCopyer) prepare(ctx context.Context, indexed <-chan *baseJob) <-chan *writeJob {
	// Everything this stage waits on runs on a context that never cancels, so a stop can
	// never release a reader before its consumer owns it.
	drained := context.WithoutCancel(ctx)

	// Disable read-ahead when a linear source must wait for each consumer.
	chanLen := 32
	if c.fromDevice.linear {
		chanLen = 0
	}

	// Collect every preparation outcome so skipped jobs can still advance linear ordering.
	completed := make(chan prepareResult, chanLen)
	prepared := make(chan *writeJob, chanLen)
	var workers sync.WaitGroup

	// Prepare source readers with the configured source-device concurrency.
	for idx := 0; idx < c.fromDevice.threads; idx++ {
		workers.Add(1)
		go c.wrap(drained, func() {
			defer workers.Done()

			// Consume indexed jobs until source exhaustion. A stopped pipeline drains its
			// input, so no accepted item is dropped.
			for job := range indexed {
				// An item that was already rejected while indexing needs no source.
				if c.markUnstarted(ctx, job) {
					if !c.sendPrepareResult(completed, prepareResult{order: job.order, job: newWriteJob(job, nil, 0, false)}) {
						return
					}
					continue
				}

				result := prepareResult{order: job.order}
				if c.toDevice.linear {
					result.release = make(chan struct{})
				}

				// Transfer the prepared item to the ordering stage before waiting on a linear
				// source.
				result.job = c.prepareItem(job)
				if !c.sendPrepareResult(completed, result) {
					return
				}
				if !result.job.waitConsumed() {
					return
				}
			}
		})
	}

	// Close preparation outcomes only after every source worker relinquishes ownership.
	go c.wrap(drained, func() {
		workers.Wait()
		close(completed)
	})

	// Preserve request order only where one linear writer requires it.
	go c.wrap(drained, func() {
		defer close(prepared)
		c.forwardPrepared(completed, prepared)
	})
	return prepared
}

func (c *StreamCopyer) sendPrepareResult(completed chan<- prepareResult, result prepareResult) bool {
	select {
	case completed <- result:
	case <-c.hardStop:
		result.finish()
		return false
	}
	if result.release == nil {
		return true
	}
	select {
	case <-result.release:
		return true
	case <-c.hardStop:
		return false
	}
}

func (result prepareResult) releaseWorker() {
	if result.release != nil {
		close(result.release)
	}
}

func (result prepareResult) finish() {
	if result.job != nil {
		result.job.finishSource()
	}
	result.releaseWorker()
}

// forwardPrepared hands prepared readers to the copy stage. A graceful stop keeps
// forwarding, so the copy stage reports every item this stage holds.
func (c *StreamCopyer) forwardPrepared(
	completed <-chan prepareResult,
	prepared chan<- *writeJob,
) {
	// Close readers that remain owned by this stage when a hard stop ends forwarding.
	defer func() {
		for result := range completed {
			result.finish()
		}
	}()

	// Random targets retain completion-order concurrency.
	if !c.toDevice.linear {
		for result := range completed {
			if result.job == nil {
				continue
			}
			select {
			case prepared <- result.job:
			case <-c.hardStop:
				result.finish()
				return
			}
		}
		return
	}

	// A bounded reorder buffer feeds the linear writer in original request order.
	next := uint64(0)
	pending := make(map[uint64]prepareResult, c.fromDevice.threads)
	defer func() {
		for _, result := range pending {
			result.finish()
		}
	}()
	for {
		select {
		case <-c.hardStop:
			return
		case result, ok := <-completed:
			if !ok {
				if len(pending) != 0 {
					c.setError(fmt.Errorf("ordered preparation ended before request %d", next))
				}
				return
			}
			pending[result.order] = result
		}

		for {
			result, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if result.job == nil {
				result.releaseWorker()
				continue
			}
			select {
			case prepared <- result.job:
				result.releaseWorker()
			case <-c.hardStop:
				result.finish()
				return
			}
		}
	}
}
