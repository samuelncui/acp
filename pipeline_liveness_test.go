package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/sirupsen/logrus"
)

// generatedReader exercises real pipeline credits without allocating a source-sized fixture.
type generatedReader struct {
	remaining int64
	read      func(int)
	closed    atomic.Bool
}

func (r *generatedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	r.remaining -= n
	if r.read != nil {
		r.read(int(n))
	}
	return int(n), nil
}

func (r *generatedReader) Close() error { r.closed.Store(true); return nil }

type pipelineFilesystem struct {
	transferFilesystem
	create func(*writeJob, targetSpec) (transferOutput, error)
}

func (f pipelineFilesystem) Create(job *writeJob, spec targetSpec) (transferOutput, error) {
	return f.create(job, spec)
}

type pipelineOutput struct {
	write  func([]byte) (int, error)
	close  func() error
	commit func() error
}

func (o *pipelineOutput) Write(p []byte) (int, error) { return o.write(p) }
func (o *pipelineOutput) Close() error {
	if o.close != nil {
		return o.close()
	}
	return nil
}
func (*pipelineOutput) Sync() error          { return nil }
func (*pipelineOutput) Cache(*baseJob, bool) {}
func (*pipelineOutput) Restore(*stat) error  { return nil }
func (*pipelineOutput) Discard() error       { return nil }
func (o *pipelineOutput) Commit(bool) error {
	if o.commit != nil {
		return o.commit()
	}
	return nil
}

func preparedFixture(c *StreamCopyer, order uint64, size int64, target string, reader io.ReadCloser) *writeJob {
	name := fmt.Sprintf("source-%d", order)
	return newWriteJob(&baseJob{copyer: c, item: &SimpleJob{Path: name, Dsts: []string{target}},
		path: name, order: order, stat: &stat{size: size}, targets: []string{target},
		outputs: []targetSpec{{name: target, path: target, device: "fixture"}}}, reader)
}

func TestPipelineFailedCreateCancelsFullPrefetchWindow(t *testing.T) {
	// Reject the target only after the source is parked beyond the complete 512 MiB window.
	synctest.Test(t, func(t *testing.T) {
		c := newTestStream(t, WithHashPolicy(HashOff), SetToDevice(LinearDevice(true)))
		prefetched := make(chan struct{})
		var once sync.Once
		var read int64
		reader := &generatedReader{remaining: readAheadBytes + batchSize, read: func(n int) {
			read += int64(n)
			if read >= readAheadBytes {
				once.Do(func() { close(prefetched) })
			}
		}}
		c.filesystem = pipelineFilesystem{create: func(*writeJob, targetSpec) (transferOutput, error) {
			<-prefetched
			synctest.Wait()
			return nil, syscall.EIO
		}}
		prepared := make(chan *writeJob, 1)
		prepared <- preparedFixture(c, 0, reader.remaining, "target", reader)
		close(prepared)

		// A transfer-local stop must finish without requiring a separate pipeline hard stop.
		var expired atomic.Bool
		guard := time.AfterFunc(time.Second, func() { expired.Store(true); c.stopHard() })
		defer guard.Stop()
		var results []Result
		for job := range c.copy(context.Background(), prepared) {
			results = append(results, job.result())
		}
		if expired.Load() || !reader.closed.Load() || len(results) != 1 {
			t.Fatalf("failed target did not drain locally: timeout=%t closed=%t results=%d", expired.Load(), reader.closed.Load(), len(results))
		}
		if !errors.Is(results[0].Targets[0].Err, ErrTargetIO) {
			t.Fatalf("target failure lost: %+v", results[0])
		}
	})
}

func TestPipelineRandomWritersReserveProgressBeforeReading(t *testing.T) {
	for _, linear := range []bool{false, true} {
		t.Run(fmt.Sprintf("linear-source=%t", linear), func(t *testing.T) {
			// Later sources cannot retain all backing while their writer has no data permit.
			synctest.Test(t, func(t *testing.T) {
				c := newTestStream(t, WithHashPolicy(HashOff), SetFromDevice(LinearDevice(linear)), SetToDevice(DeviceThreads(1)))
				started, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				var laterReads, written atomic.Int64
				c.filesystem = pipelineFilesystem{create: func(job *writeJob, _ targetSpec) (transferOutput, error) {
					return &pipelineOutput{write: func(data []byte) (int, error) {
						if job.order == 0 {
							once.Do(func() { close(started) })
							<-release
						}
						written.Add(int64(len(data)))
						return len(data), nil
					}}, nil
				}}
				const size = readAheadBytes + batchSize
				prepared := make(chan *writeJob, 3)
				var readers []*generatedReader
				for i := range 3 {
					r := &generatedReader{remaining: size}
					if i != 0 {
						r.read = func(n int) { laterReads.Add(int64(n)) }
					}
					readers = append(readers, r)
					prepared <- preparedFixture(c, uint64(i), size, fmt.Sprintf("target-%d", i), r)
				}
				close(prepared)
				out := c.copy(context.Background(), prepared)
				<-started
				synctest.Wait()
				if n := laterReads.Load(); n != 0 {
					t.Errorf("unadmitted readers retained %d bytes", n)
				}

				// Once the first writer resumes, every source and target must settle without losing bytes.
				close(release)
				count := 0
				for job := range out {
					count++
					result := job.result()
					if result.Err != nil || result.Targets[0].Err != nil {
						t.Errorf("unexpected copy failure: %+v", result)
					}
				}
				if count != 3 || written.Load() != 3*size {
					t.Fatalf("incomplete copies: files=%d bytes=%d", count, written.Load())
				}
				for _, r := range readers {
					if !r.closed.Load() {
						t.Fatal("source was not closed")
					}
				}
			})
		})
	}
}

func TestPipelineFailedMiddleTargetPreservesPublicationOrder(t *testing.T) {
	for _, failure := range []string{"create", "write", "close"} {
		t.Run(failure, func(t *testing.T) {
			// Hold A's Close while B fails and C finishes its data for the same target path.
			synctest.Test(t, func(t *testing.T) {
				c := newTestStream(t, WithHashPolicy(HashOff), SetToDevice(LinearDevice(true)))
				release, thirdWrite := make(chan struct{}), make(chan struct{})
				commits := make(chan uint64, 3)
				var once sync.Once
				c.filesystem = pipelineFilesystem{create: func(job *writeJob, _ targetSpec) (transferOutput, error) {
					if job.order == 1 && failure == "create" {
						return nil, syscall.EIO
					}
					return &pipelineOutput{
						write: func(data []byte) (int, error) {
							if job.order == 2 {
								once.Do(func() { close(thirdWrite) })
							}
							if job.order == 1 && failure == "write" {
								return 0, syscall.EIO
							}
							return len(data), nil
						},
						close: func() error {
							if job.order == 0 {
								<-release
							}
							if job.order == 1 && failure == "close" {
								return syscall.EIO
							}
							return nil
						},
						commit: func() error { commits <- job.order; return nil },
					}, nil
				}}
				prepared := make(chan *writeJob, 3)
				for i := range 3 {
					prepared <- preparedFixture(c, uint64(i), 4096, "same-target", &generatedReader{remaining: 4096})
				}
				close(prepared)
				out := c.copy(context.Background(), prepared)
				<-thirdWrite
				synctest.Wait()
				if len(commits) != 0 {
					t.Error("C published before A's pending completion")
				}

				// A failed middle entry must not let an older successful copy overwrite the newest one.
				close(release)
				failures, count := 0, 0
				for job := range out {
					count++
					if job.result().Targets[0].Err != nil {
						failures++
					}
				}
				close(commits)
				var order []uint64
				for i := range commits {
					order = append(order, i)
				}
				if count != 3 || failures != 1 || len(order) != 2 || order[0] != 0 || order[1] != 2 {
					t.Fatalf("wrong same-path outcome: count=%d failures=%d publications=%v", count, failures, order)
				}
			})
		})
	}
}

// panicLogHook represents a caller-supplied logger that fails while an I/O error is reported.
type panicLogHook struct{}

func (panicLogHook) Levels() []logrus.Level   { return logrus.AllLevels }
func (panicLogHook) Fire(*logrus.Entry) error { panic("injected log hook panic") }

func TestPipelinePublicationReleasesAfterLoggingPanic(t *testing.T) {
	// Both outputs already own publication tickets when the first Close reports an error.
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.AddHook(panicLogHook{})
	c := newTestStream(t, WithHashPolicy(HashOff), WithLogger(logger), SetToDevice(LinearDevice(true)))
	order := newPublicationOrder()
	first, second := order.reserve("same-target"), order.reserve("same-target")
	job := preparedFixture(c, 0, 0, "same-target", &generatedReader{})
	tr := newTransfer(c, job, newReadAhead(c), 0)
	target := &targetTransfer{spec: job.outputs[0], publication: first,
		output: &pipelineOutput{close: func() error { return syscall.EIO }}}

	// Even a secondary panic in failure reporting must release the next file's dependency.
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		tr.finishTarget(target, mapset.NewSet[string]())
	}()
	if recovered == nil {
		t.Fatal("error logger did not exercise the panic boundary")
	}
	select {
	case <-second.previous:
		second.release()
	default:
		t.Fatal("logging panic stranded the next publication")
	}
}
