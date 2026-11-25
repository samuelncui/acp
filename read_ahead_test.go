package acp

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestPipelineSingleReaderPrefetchesAfterSourceEOF(t *testing.T) {
	// Both random and linear sources can read sequentially ahead of a blocked linear writer.
	for _, linear := range []bool{false, true} {
		t.Run(fmt.Sprintf("linear-source=%t", linear), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newTestStream(t, WithHashPolicy(HashRead),
					SetFromDevice(DeviceThreads(1), LinearDevice(linear)), SetToDevice(LinearDevice(true)))
				gate, writing := make(chan struct{}), make(chan struct{})
				release := onceRelease(gate)
				defer release()
				var written atomic.Int64
				c.filesystem = pipelineFilesystem{create: func(job *writeJob, _ targetSpec) (transferOutput, error) {
					return &pipelineOutput{write: func(p []byte) (int, error) {
						if job.order == 0 {
							close(writing)
							<-gate
						}
						written.Add(int64(len(p)))
						return len(p), nil
					}}, nil
				}}
				const size = 4096
				var reads [3]atomic.Int64
				var readers [3]*generatedReader
				prepared := make(chan *writeJob, len(readers))
				for i := range readers {
					readers[i] = &generatedReader{remaining: size, read: func(n int) { reads[i].Add(int64(n)) }}
					prepared <- preparedFixture(c, uint64(i), size, fmt.Sprintf("target-%d", i), readers[i])
				}
				close(prepared)
				out := c.copy(context.Background(), prepared)
				<-writing
				synctest.Wait()

				// EOF moves the read reservation while Write remains blocked and the byte window has room.
				for i, reader := range readers {
					if reads[i].Load() != size || !reader.closed.Load() {
						t.Errorf("source %d still waits for Write: bytes=%d closed=%t", i, reads[i].Load(), reader.closed.Load())
					}
				}

				// Releasing the writer settles every hash and target without changing output accounting.
				release()
				count := 0
				for job := range out {
					count++
					result := job.result()
					if result.Err != nil || result.Targets[0].Err != nil || len(result.SHA256) != 32 {
						t.Errorf("incomplete result: %+v", result)
					}
				}
				if count != len(readers) || written.Load() != int64(len(readers)*size) {
					t.Fatalf("incomplete transfer: files=%d bytes=%d", count, written.Load())
				}
			})
		})
	}
}

func TestReadAheadPrioritySkipsCompletedSourcesInOrder(t *testing.T) {
	// Later empty or failed sources may finish before the foreground source has read its data.
	synctest.Test(t, func(t *testing.T) {
		c := newTestStream(t, SetFromDevice(DeviceThreads(1)), SetToDevice(LinearDevice(true)))
		ahead := newReadAhead(c)
		future := make(chan struct{})
		go func() {
			release, err := ahead.readSlot(3, nil)
			if err == nil {
				release()
			}
			close(future)
		}()
		ahead.finishRead(2)
		ahead.finishRead(1)
		synctest.Wait()
		select {
		case <-future:
			t.Fatal("completed later sources skipped the unread foreground source")
		default:
		}

		// Only completion of the earliest source releases the next remaining source's Read.
		release, err := ahead.readSlot(0, nil)
		if err != nil {
			t.Fatal(err)
		}
		release()
		ahead.finishRead(0)
		<-future
	})
}

func TestReadAheadWindowAdmitsPrefixOfNextLargeFile(t *testing.T) {
	// A four-GiB head exposes only the next file's eligible prefix as the writer approaches its end.
	c := newTestStream(t, SetToDevice(LinearDevice(true)))
	ahead := newReadAhead(c)
	const next = int64(4 << 30)
	const prefix = int64(100 << 20)
	ahead.advance(0, next-readAheadBytes+prefix)
	for offset := int64(0); offset < prefix; offset += batchSize {
		n, err := ahead.wait(1, next+offset, batchSize, false, nil)
		if err != nil || n != batchSize {
			t.Fatalf("prefix at %d: %d/%v", offset, n, err)
		}
	}
	blocked := make(chan struct{})
	go func() { _, _ = ahead.wait(1, next+prefix, batchSize, false, nil); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("prefetch exceeded the logical window")
	default:
	}
	ahead.advance(0, next-readAheadBytes+prefix+batchSize)
	awaitStage(t, blocked, "sliding window eligibility")
}

func TestReadAheadSingleReaderReservesHeadOpportunity(t *testing.T) {
	// The sole read permit cannot be occupied by speculative work ahead of the ordered head.
	synctest.Test(t, func(t *testing.T) {
		c := &StreamCopyer{option: &option{fromDevice: &deviceOption{threads: 1}, toDevice: &deviceOption{linear: true}}, hardStop: make(chan struct{})}
		ahead := newReadAhead(c)
		future := make(chan struct{})
		go func() { release, _ := ahead.readSlot(1, nil); release(); close(future) }()
		synctest.Wait()
		select {
		case <-future:
			t.Fatal("future took the head's only read slot")
		default:
		}

		// The foreground source retains priority until its reading finishes, independent of Write.
		release, err := ahead.readSlot(0, nil)
		if err != nil {
			t.Fatal(err)
		}
		release()
		ahead.finishRead(0)
		<-future
	})
}

func TestReadAheadBackingCreditsWaitForLastConsumer(t *testing.T) {
	// Exhaust speculative backing capacity while retaining the foreground reservation.
	synctest.Test(t, func(t *testing.T) {
		c := &StreamCopyer{option: &option{fromDevice: &deviceOption{threads: 8}, toDevice: &deviceOption{linear: true}}, hardStop: make(chan struct{})}
		ahead := newReadAhead(c)
		var held []*chunkBuffer
		for range readAheadChunks - headChunks {
			chunk, err := ahead.acquire(1, nil)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, chunk)
		}
		blocked := make(chan *chunkBuffer, 1)
		go func() { chunk, _ := ahead.acquire(1, nil); blocked <- chunk }()
		synctest.Wait()
		select {
		case <-blocked:
			t.Fatal("future consumed the foreground reservation")
		default:
		}
		for range headChunks {
			chunk, err := ahead.acquire(0, nil)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, chunk)
		}

		// A shared reference remains charged until both consumers release the same backing allocation.
		shared := held[0].retain()
		held[0].release()
		held[0] = nil
		synctest.Wait()
		select {
		case <-blocked:
			t.Fatal("backing credit released before the final consumer")
		default:
		}
		shared.release()
		replacement := <-blocked
		if cap(replacement.data) != batchSize {
			t.Fatalf("backing grew outside budget: capacity=%d", cap(replacement.data))
		}
		replacement.release()
		for _, chunk := range held {
			if chunk != nil {
				chunk.release()
			}
		}

		// Every credit came back: the whole budget can be taken again without waiting.
		whole := make([]*chunkBuffer, 0, readAheadChunks)
		for range readAheadChunks {
			chunk, err := ahead.acquire(0, nil)
			if err != nil {
				t.Fatal(err)
			}
			whole = append(whole, chunk)
		}
		for _, chunk := range whole {
			chunk.release()
		}

		// A settled stream must detach pooled backing so it cannot retain the old stream and its jobs.
		ahead.close()
		for _, chunk := range append(held, shared, replacement) {
			if chunk != nil && (chunk.owner != nil || chunk.future || chunk.refs != 0) {
				t.Fatalf("settled backing still owns stream resources: owner=%p future=%t refs=%d", chunk.owner, chunk.future, chunk.refs)
			}
		}
	})
}
