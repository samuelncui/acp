package acp

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"sync"
	"sync/atomic"

	mapset "github.com/deckarep/golang-set/v2"
)

// transfer owns a file's buffers, concurrent consumers and terminal outcomes.
type transfer struct {
	copyer                                                     *StreamCopyer
	job                                                        *writeJob
	ahead                                                      *readAhead
	start                                                      int64
	chunks                                                     chan *chunkBuffer
	hashes                                                     chan *chunkBuffer
	readDone, hashDone, sourceDone, sourceCacheDone, writeDone chan struct{}
	readTurn, writeTurn                                        <-chan struct{}
	stop                                                       chan struct{}
	stopOnce, hashOnce                                         sync.Once
	readErr, sourceErr, hashErr                                error
	digest                                                     []byte
	noContent                                                  bool
	admitted                                                   bool
	publications                                               []publicationTurn
	targets                                                    sync.WaitGroup
}

func newTransfer(c *StreamCopyer, job *writeJob, ahead *readAhead, start int64) *transfer {
	// Channels hold references; backing capacity is shared across every admitted transfer.
	size := int64(0)
	if job.stat != nil {
		size = max(job.stat.size, 0)
	}
	capacity := max(1, int((min(size, readAheadBytes)+batchSize-1)/batchSize))
	t := &transfer{
		copyer: c, job: job, ahead: ahead, start: start,
		chunks:   make(chan *chunkBuffer, capacity),
		readDone: make(chan struct{}), hashDone: make(chan struct{}), sourceDone: make(chan struct{}),
		sourceCacheDone: make(chan struct{}), writeDone: make(chan struct{}), stop: make(chan struct{}),
		noContent: job.itemError != nil || job.skipContent || job.cacheHit || job.reader == nil,
	}
	if !t.noContent && c.hashPolicy.producesHash() {
		t.hashes = make(chan *chunkBuffer, 4)
	}
	return t
}

func (t *transfer) abort() { t.stopOnce.Do(func() { close(t.stop) }) }

func (t *transfer) panicFailure(stage string, value any) error {
	err := panicError(stage, value)
	t.copyer.setError(err)
	t.copyer.stopHard()
	return err
}

func (t *transfer) wait(turn <-chan struct{}) bool {
	if turn == nil {
		return true
	}
	select {
	case <-turn:
		return true
	case <-t.copyer.hardStop:
		return false
	}
}

func (t *transfer) hashContent() {
	// Digest completion cannot strand a target after a hash panic.
	defer close(t.hashDone)
	if t.hashes == nil {
		return
	}
	defer func() {
		if value := recover(); value != nil {
			t.hashErr = t.panicFailure("hash consumer", value)
		}
		for chunk := range t.hashes {
			chunk.release()
		}
	}()
	sha := sha256Pool.Get().(hash.Hash)
	defer sha256Pool.Put(sha)
	sha.Reset()
	for chunk := range t.hashes {
		func() { defer chunk.release(); _, _ = sha.Write(chunk.data) }()
	}
	if t.readErr == nil {
		t.digest = sha.Sum(nil)
	}
}

func (t *transfer) publishHash() {
	// Only logically admitted work publishes a computed digest into public item facts.
	<-t.hashDone
	<-t.readDone
	t.hashOnce.Do(func() {
		if t.noContent {
			return
		}
		t.job.setHash(t.digest)
		if t.readErr == nil && t.hashErr == nil {
			t.job.validateHash()
		}
	})
}

func (t *transfer) cacheSource() {
	// Source path identity remains stable until rewrite publication observes this completion.
	defer close(t.sourceCacheDone)
	defer func() {
		if value := recover(); value != nil {
			t.panicFailure("source cache", value)
		}
	}()
	t.publishHash()
	<-t.sourceDone
	if t.job.sourcePath != "" {
		t.copyer.refreshCachePath(t.job.sourcePath, t.job.baseJob, false)
	}
}

func (t *transfer) run(ctx context.Context, out chan<- *baseJob, writing chan struct{}, cntr *counter, exhausted mapset.Set[string]) {
	// Reading and hashing can run ahead while target data remains ordered.
	c := t.copyer
	drained := context.WithoutCancel(ctx)
	go c.wrap(drained, t.hashContent)
	go c.wrap(drained, t.readContent)
	active, turnReleased := false, false
	defer func() {
		if value := recover(); value != nil {
			t.job.itemError = t.panicFailure("transfer", value)
		}
		t.abort()
		if !turnReleased {
			t.finishWrite()
		}
		for chunk := range t.chunks {
			chunk.release()
		}
		<-t.readDone
		<-t.hashDone
		<-t.sourceDone
		if active {
			t.cacheSource()
		}

		// Failed or unstarted outputs release in reservation order after rewrite's cache dependency.
		for _, turn := range t.publications {
			if turn.release != nil {
				turn.release()
			}
		}
		t.targets.Wait()
		t.job.itemError = errors.Join(t.job.itemError, t.sourceErr)
		c.publish(out, t.job.baseJob)
	}()

	// The public graceful-stop boundary remains logical Copy admission, not speculative Read.
	if !t.wait(t.writeTurn) {
		return
	}
	if !t.admitted {
		select {
		case writing <- struct{}{}:
		case <-c.hardStop:
			return
		}
	}
	held := true
	defer func() {
		if held {
			<-writing
		}
	}()
	if c.markUnstarted(ctx, t.job.baseJob) {
		return
	}
	t.ahead.advance(t.job.order, t.start)
	t.job.startWrite()
	active = true
	atomic.AddInt64(&cntr.files, 1)

	// Targetless and cache-hit work still consumes bounded read progress without a target writer.
	if t.noContent {
		if t.job.cacheHit && t.job.stat != nil {
			atomic.AddInt64(&cntr.bytes, t.job.stat.size)
		}
	} else {
		t.writeContent(cntr, exhausted)
	}
	<-writing
	held = false
	t.finishWrite()
	turnReleased = true
}

func (t *transfer) finishWrite() {
	// The next data producer need not wait for hash, Close, metadata or results.
	end := t.start
	if t.job.stat != nil {
		end += max(t.job.stat.size, 0)
	}
	t.ahead.advance(t.job.order+1, end)
	close(t.writeDone)
}

func (t *transfer) stopped() bool {
	select {
	case <-t.stop:
		return true
	case <-t.copyer.hardStop:
		return true
	default:
		return false
	}
}

func (t *transfer) stoppedError() error { return fmt.Errorf("source prefetch stopped") }
