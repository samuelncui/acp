package acp

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	sha256 "github.com/minio/sha256-simd"
)

const batchSize = 1 * 1024 * 1024

var sha256Pool = &sync.Pool{New: func() interface{} { return sha256.New() }}

// chunkBuffer shares immutable content; the final reference returns one backing allocation.
type chunkBuffer struct {
	data   []byte
	refs   int32
	owner  *readAhead
	future bool
}

// New is also the private allocation seam used by resource-lifetime checks.
var chunkPool = sync.Pool{New: func() interface{} {
	return &chunkBuffer{data: make([]byte, batchSize)}
}}

func (c *chunkBuffer) retain() *chunkBuffer {
	atomic.AddInt32(&c.refs, 1)
	return c
}

func (c *chunkBuffer) release() {
	if atomic.AddInt32(&c.refs, -1) != 0 {
		return
	}
	if c.owner != nil {
		c.owner.release(c)
		return
	}
	chunkPool.Put(c)
}

// copy admits bounded file lifetimes while data lanes and completion run independently.
func (c *StreamCopyer) copy(ctx context.Context, prepared <-chan *writeJob) <-chan *baseJob {
	// One owner dispatches transfer order; public result delivery remains independent.
	out := make(chan *baseJob, 128)
	ahead := newReadAhead(c)
	writing := make(chan struct{}, max(c.toDevice.threads, 1))
	exhausted := mapset.NewSet[string]()
	cntr := new(counter)
	done := make(chan struct{})
	reported := make(chan struct{})
	go c.wrap(context.WithoutCancel(ctx), func() {
		defer close(reported)
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

	// Managed children retain their own resources until completion; ordered turns end at data EOF.
	go c.wrap(context.WithoutCancel(ctx), func() {
		var children sync.WaitGroup
		defer func() {
			// Return backing only after all data, completion and cleanup owners have exited.
			children.Wait()
			ahead.close()
			close(done)
			<-reported
			close(out)
		}()
		ready := make(chan struct{})
		close(ready)
		var readTurn, writeTurn <-chan struct{} = ready, ready
		var offset int64
		publication := newPublicationOrder()
		for job := range prepared {
			t := newTransfer(c, job, ahead, offset)
			if job.stat != nil {
				offset += max(job.stat.size, 0)
			}
			if c.fromDevice.linear {
				t.readTurn, readTurn = readTurn, t.readDone
			}
			if c.toDevice.linear {
				t.writeTurn, writeTurn = writeTurn, t.writeDone
			}
			t.reservePublications(publication)

			// Reserve random-target writers in source dispatch order before their readers can hold backing.
			// Linear targets instead keep speculative readers ahead of their single ordered data lane.
			if !c.toDevice.linear {
				select {
				case writing <- struct{}{}:
					t.admitted = true
				case <-c.hardStop:
				}
			}
			children.Add(1)
			go c.wrap(context.WithoutCancel(ctx), func() {
				defer children.Done()
				defer job.releaseSlot()
				t.run(ctx, out, writing, cntr, exhausted)
			})
		}
	})
	return out
}
