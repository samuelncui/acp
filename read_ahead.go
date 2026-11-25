package acp

import (
	"fmt"
	"sync"
)

const (
	transferLimit   = 256
	readAheadBytes  = 512 << 20
	readAheadChunks = readAheadBytes / batchSize
	headChunks      = 8
)

// readAhead owns one run's backing allocations, logical window and read permits.
// Its reservations protect the ordered head without changing item feed capacity.
type readAhead struct {
	copyer  *StreamCopyer
	ordered bool
	readers int

	lock          sync.Mutex
	changed       chan struct{}
	free          chan *chunkBuffer
	head          uint64
	readHead      uint64
	readDone      map[uint64]struct{}
	position      int64
	used          int
	futureUsed    int
	reading       int
	futureReading int
}

func newReadAhead(c *StreamCopyer) *readAhead {
	// Preserve configured source concurrency, including single-reader sources.
	readers := c.fromDevice.threads
	if readers < 1 {
		readers = 1
	}
	return &readAhead{
		copyer: c, ordered: c.toDevice.linear, readers: readers,
		free: make(chan *chunkBuffer, readAheadChunks),
	}
}

var (
	errReadAheadTransfer = fmt.Errorf("read-ahead stopped by transfer")
	errReadAheadPipeline = fmt.Errorf("read-ahead stopped by pipeline failure")
)

// await runs try under the budget lock until it reports done, parking between attempts until the
// budget changes. Either stop releases a parked call without another budget change, and a stop is
// observed before every attempt. try must not unlock the budget lock.
func (b *readAhead) await(stop <-chan struct{}, try func() bool) error {
	for {
		// Observe the frontier and subscribe before releasing the lock.
		b.lock.Lock()
		select {
		case <-stop:
			b.lock.Unlock()
			return errReadAheadTransfer
		case <-b.copyer.hardStop:
			b.lock.Unlock()
			return errReadAheadPipeline
		default:
		}
		if try() {
			b.lock.Unlock()
			return nil
		}
		changed := b.subscribe()
		b.lock.Unlock()

		select {
		case <-changed:
		case <-stop:
			return errReadAheadTransfer
		case <-b.copyer.hardStop:
			return errReadAheadPipeline
		}
	}
}

func (b *readAhead) wait(
	order uint64, position int64, want int, eof bool, stop <-chan struct{},
) (int, error) {
	// Limit a read operation independently of its backing allocation.
	if want <= 0 {
		return 0, nil
	}
	want = min(want, batchSize)
	allowed := 0
	err := b.await(stop, func() bool {
		if !b.ordered || (eof && order <= b.head) {
			allowed = want
			if eof {
				allowed = 1
			}
			return true
		}
		distance := position - b.position
		if distance >= readAheadBytes {
			return false
		}
		allowed = want
		if distance > 0 {
			allowed = min(allowed, int(readAheadBytes-distance))
		}
		return true
	})
	return allowed, err
}

func (b *readAhead) acquire(order uint64, stop <-chan struct{}) (*chunkBuffer, error) {
	var chunk *chunkBuffer
	err := b.await(stop, func() bool {
		// Allocate lazily under the same owner that accounts live backing credits.
		future := b.ordered && order > b.head
		if b.used >= readAheadChunks || (future && b.futureUsed >= readAheadChunks-headChunks) {
			return false
		}
		select {
		case chunk = <-b.free:
		default:
			chunk = chunkPool.Get().(*chunkBuffer)
		}
		b.used++
		if future {
			b.futureUsed++
		}
		chunk.data = chunk.data[:cap(chunk.data)]
		chunk.refs = 1
		chunk.owner = b
		chunk.future = future
		return true
	})
	return chunk, err
}

func (b *readAhead) release(chunk *chunkBuffer) {
	// Last-reference release returns backing storage only to this run's bounded pool.
	b.lock.Lock()
	defer b.lock.Unlock()
	b.used--
	if chunk.future {
		b.futureUsed--
	}
	chunk.future = false
	b.free <- chunk
	b.notify()
}

func (b *readAhead) close() {
	// The dispatcher joins every child before returning this run's unreferenced backing.
	for {
		select {
		case chunk := <-b.free:
			chunk.owner = nil
			chunk.future = false
			chunkPool.Put(chunk)
		default:
			return
		}
	}
}

func (b *readAhead) advance(order uint64, position int64) {
	// Random targets do not constrain reads by a consumed-content frontier.
	if !b.ordered {
		return
	}

	// Only the ordered writer moves the head and consumed-content frontier.
	b.lock.Lock()
	defer b.lock.Unlock()
	if order < b.head || (order == b.head && position <= b.position) {
		return
	}
	b.head = order
	b.position = max(b.position, position)
	b.notify()
}

func (b *readAhead) finishRead(order uint64) {
	// Reader priority follows completed sources, independently of the writer's byte frontier.
	if !b.ordered {
		return
	}
	b.lock.Lock()
	defer b.lock.Unlock()
	if order != b.readHead {
		if b.readDone == nil {
			b.readDone = make(map[uint64]struct{})
		}
		b.readDone[order] = struct{}{}
		return
	}

	// Completed speculative or empty sources cannot retain the next reader's reservation.
	b.readHead++
	for {
		if _, done := b.readDone[b.readHead]; !done {
			break
		}
		delete(b.readDone, b.readHead)
		b.readHead++
	}
	b.notify()
}

func (b *readAhead) readSlot(order uint64, stop <-chan struct{}) (func(), error) {
	var future bool
	err := b.await(stop, func() bool {
		// Reserve one dispatch opportunity for the ordered head when concurrency permits.
		future = b.ordered && order > b.readHead
		reserved := future && b.futureReading >= b.readers-1
		if b.reading >= b.readers || reserved {
			return false
		}
		b.reading++
		if future {
			b.futureReading++
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	reservedFuture := future
	return func() {
		// The caller releases immediately after Read, before any channel wait.
		b.lock.Lock()
		defer b.lock.Unlock()
		b.reading--
		if reservedFuture {
			b.futureReading--
		}
		b.notify()
	}, nil
}

// subscribe observes the next state change while the caller holds the budget lock.
func (b *readAhead) subscribe() <-chan struct{} {
	if b.changed == nil {
		b.changed = make(chan struct{})
	}
	return b.changed
}

// notify releases every current waiter; the next waiter allocates its own notification.
// The caller holds the budget lock across its state change and this notification.
func (b *readAhead) notify() {
	if b.changed == nil {
		return
	}
	close(b.changed)
	b.changed = nil
}
