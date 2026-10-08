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
}

// openSourceContent opens the single descriptor an item owns, together with the reader that
// delivers its content. It is a variable so a test can observe which descriptor an item's cache
// read and cache write use.
var openSourceContent = func(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
	// Only a caller that asks for it reads through a mapping, which retains its descriptor.
	if mode == ReadMapped {
		readerAt, err := mmap.OpenWithInfo(path, info)
		if err != nil {
			return itemSource{}, fmt.Errorf("open src file by mmap fail, %w", err)
		}

		return itemSource{
			file:   readerAt.File(),
			reader: mmap.NewReader(readerAt),
		}, nil
	}

	// Every other source is read buffered, without updating its access time.
	file, err := openSource(path)
	if err != nil {
		return itemSource{}, fmt.Errorf("open src file fail, %w", err)
	}
	return itemSource{file: file, reader: file}, nil
}

// prepareItem opens the one descriptor an item owns and reads its stored hash through it. That
// descriptor supplies cached metadata and content once; computed cache refresh can follow closure by path.
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
	source, err := c.fs().Open(job.path, mode, job.stat.info)
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
		return newWriteJob(job, nil)
	}

	// Retain ownership until the prepared job takes it, including a cache lookup panic.
	defer func() {
		if source.reader != nil {
			_ = source.reader.Close()
		}
	}()

	// Read the source entry once. Reuse accounts for misses and warnings; refresh-only reads
	// silently replace unusable entries, preserving that policy's existing diagnostics.
	reused := false
	if reuses {
		if hash, ok := c.signatures.lookup(source.file, job.path, job.stat); ok {
			job.setCachedHash(hash)
			reused = true
		}
	} else if c.signatures != nil && c.hashPolicy.refreshesCache() {
		if signature, status, err := readCachedSignatureInfo(source.file, job.stat.info); err == nil && status == signatureReadHit {
			job.cachedSignature = &signature
		}
	}

	// Transfer the descriptor and indexed facts to the content stage.
	wj := newWriteJob(job, source.reader)
	wj.source = source.file
	wj.sourcePath = job.path
	source.reader = nil
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
	wj := newWriteJob(job, nil)
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

	// Bound opened sources through their complete transfer lifetime, independently of data lanes.
	chanLen := 32
	slots := make(chan struct{}, transferLimit)

	// Collect every preparation outcome so skipped jobs can still advance linear ordering.
	completed := make(chan prepareResult, chanLen)
	prepared := make(chan *writeJob, chanLen)
	var workers sync.WaitGroup

	// Prepare source readers with the configured source-device concurrency.
	for idx := 0; idx < min(max(c.fromDevice.threads, 8), transferLimit); idx++ {
		workers.Add(1)
		go c.wrap(drained, func() {
			defer workers.Done()

			// Consume indexed jobs until source exhaustion. A stopped pipeline drains its
			// input, so no accepted item is dropped.
			for {
				// Reserve admission before dequeue so an earlier job cannot wait behind later jobs.
				select {
				case slots <- struct{}{}:
				case <-c.hardStop:
					return
				}
				// An idle reservation owns no job and must return its slot on exhaustion or failure.
				var job *baseJob
				select {
				case next, ok := <-indexed:
					if !ok {
						<-slots
						return
					}
					job = next
				case <-c.hardStop:
					<-slots
					return
				}
				result := prepareResult{order: job.order, release: make(chan struct{})}
				transferred := false
				func() {
					defer func() {
						if !transferred {
							<-slots
						}
					}()
					if c.markUnstarted(ctx, job) {
						result.job = newWriteJob(job, nil)
					} else {
						result.job = c.prepareItem(job)
					}
					result.job.release = func() { <-slots }
					transferred = true
				}()
				if !c.sendPrepareResult(completed, result) {
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
		result.job.releaseSlot()
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

	// Concurrent random devices retain completion-order preparation.
	serial := c.fromDevice.threads == 1 && c.toDevice.threads == 1
	if !c.toDevice.linear && !c.fromDevice.linear && !serial {
		for result := range completed {
			if result.job == nil {
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
		return
	}

	// A bounded reorder buffer retains the request order of formerly serialized devices.
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
