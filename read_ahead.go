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
	allocated     int
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

func (b *readAhead) wait(
	order uint64, position int64, want int, eof bool, stop <-chan struct{},
) (int, error) {
	// Limit a read operation independently of its backing allocation.
	if want <= 0 {
		return 0, nil
	}
	want = min(want, batchSize)
	for {
		// Observe the frontier and subscribe before releasing its lock.
		b.lock.Lock()
		select {
		case <-stop:
			b.lock.Unlock()
			return 0, fmt.Errorf("read-ahead stopped by transfer")
		case <-b.copyer.hardStop:
			b.lock.Unlock()
			return 0, fmt.Errorf("read-ahead stopped by pipeline failure")
		default:
		}
		if !b.ordered || (eof && order <= b.head) {
			b.lock.Unlock()
			if eof {
				return 1, nil
			}
			return want, nil
		}
		distance := position - b.position
		if distance < readAheadBytes {
			allowed := want
			if distance > 0 {
				allowed = min(allowed, int(readAheadBytes-distance))
			}
			b.lock.Unlock()
			return allowed, nil
		}
		changed := b.subscribe()
		b.lock.Unlock()

		// Either stop releases parked reads without waiting for another frontier change.
		select {
		case <-changed:
		case <-stop:
			return 0, fmt.Errorf("read-ahead stopped by transfer")
		case <-b.copyer.hardStop:
			return 0, fmt.Errorf("read-ahead stopped by pipeline failure")
		}
	}
}

func (b *readAhead) acquire(order uint64, stop <-chan struct{}) (*chunkBuffer, error) {
	for {
		// Allocate lazily under the same owner that accounts live backing credits.
		b.lock.Lock()
		select {
		case <-stop:
			b.lock.Unlock()
			return nil, fmt.Errorf("read-ahead stopped by transfer")
		case <-b.copyer.hardStop:
			b.lock.Unlock()
			return nil, fmt.Errorf("read-ahead stopped by pipeline failure")
		default:
		}
		future := b.ordered && order > b.head
		if b.used < readAheadChunks && (!future || b.futureUsed < readAheadChunks-headChunks) {
			var chunk *chunkBuffer
			select {
			case chunk = <-b.free:
			default:
				// A test allocation hook may panic; never leave the owner locked.
				func() {
					defer func() {
						if value := recover(); value != nil {
							b.lock.Unlock()
							panic(value)
						}
					}()
					chunk = chunkPool.Get().(*chunkBuffer)
				}()
				b.allocated++
			}
			b.used++
			if future {
				b.futureUsed++
			}
			chunk.data = chunk.data[:cap(chunk.data)]
			chunk.refs = 1
			chunk.owner = b
			chunk.future = future
			b.lock.Unlock()
			return chunk, nil
		}
		changed := b.subscribe()
		b.lock.Unlock()

		// Head promotion or either stop can release a backing wait without another buffer release.
		select {
		case <-changed:
		case <-stop:
			return nil, fmt.Errorf("read-ahead stopped by transfer")
		case <-b.copyer.hardStop:
			return nil, fmt.Errorf("read-ahead stopped by pipeline failure")
		}
	}
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
	for {
		// Reserve one dispatch opportunity for the ordered head when concurrency permits.
		b.lock.Lock()
		select {
		case <-stop:
			b.lock.Unlock()
			return nil, fmt.Errorf("read-ahead stopped by transfer")
		case <-b.copyer.hardStop:
			b.lock.Unlock()
			return nil, fmt.Errorf("read-ahead stopped by pipeline failure")
		default:
		}
		future := b.ordered && order > b.readHead
		reserved := future && b.futureReading >= b.readers-1
		if b.reading < b.readers && !reserved {
			b.reading++
			if future {
				b.futureReading++
			}
			b.lock.Unlock()
			return func() {
				// The caller releases immediately after Read, before any channel wait.
				b.lock.Lock()
				defer b.lock.Unlock()
				b.reading--
				if future {
					b.futureReading--
				}
				b.notify()
			}, nil
		}
		changed := b.subscribe()
		b.lock.Unlock()

		// Read completion, head promotion or either stop wakes callers without polling.
		select {
		case <-changed:
		case <-stop:
			return nil, fmt.Errorf("read-ahead stopped by transfer")
		case <-b.copyer.hardStop:
			return nil, fmt.Errorf("read-ahead stopped by pipeline failure")
		}
	}
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
