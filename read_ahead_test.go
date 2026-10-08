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
