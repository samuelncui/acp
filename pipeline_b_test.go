package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func awaitStage(t *testing.T, ch <-chan struct{}, stage string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", stage)
	}
}

func onceRelease(ch chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func TestPipelineOverlapsHashWriteAndLinearClose(t *testing.T) {
	// Hold hashing and the first Close independently while both ordered data writes progress.
	root := t.TempDir()
	content := bytes.Repeat([]byte("parallel content"), 64<<10)
	source := writeSourceFile(t, root, "source", content)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	hashStarted, _, releaseHash := holdContentHash(t)
	t.Cleanup(releaseHash)
	closeStarted, secondWrite, closeGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseClose := onceRelease(closeGate)
	t.Cleanup(releaseClose)
	var closeOnce, writeOnce sync.Once
	model := newLTFSModel(ltfsModelConfig{capture: true, entered: func(op, path string) {
		if op == "close" && path == a {
			closeOnce.Do(func() { close(closeStarted) })
			<-closeGate
		}
		if op == "write" && path == b {
			writeOnce.Do(func() { close(secondWrite) })
		}
	}})
	defer model.shutdown()
	results := make(chan Result, 2)
	c, err := NewStream(context.Background(), func(batch []Result) error {
		for _, r := range batch {
			results <- r
		}
		return nil
	}, WithHashPolicy(HashRead), WithResultBatch(1), SetToDevice(LinearDevice(true)))
	if err != nil {
		t.Fatal(err)
	}
	c.filesystem = ltfsFilesystem{transferFilesystem: c.fs(), model: model}
	if err := c.Submit(&SimpleJob{Path: source, Dsts: []string{a}}, &SimpleJob{Path: source, Dsts: []string{b}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()

	// A's Close and B's Write both start while A's digest remains unavailable.
	awaitStage(t, hashStarted, "hash consumer")
	awaitStage(t, closeStarted, "first Close before hash completion")
	awaitStage(t, secondWrite, "second Write before first Close completion")
	select {
	case r := <-results:
		t.Fatalf("premature result: %+v", r)
	default:
	}
	releaseClose()
	releaseHash()
	awaitStage(t, done, "pipeline drain")
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := model.validate(); err != nil {
		t.Fatal(err)
	}

	// The same bytes reach both destinations once, while linear application Write never overlaps.
	want := sha256.Sum256(content)
	for range 2 {
		r := <-results
		if r.Err != nil || r.Targets[0].Err != nil || !bytes.Equal(r.SHA256, want[:]) {
			t.Fatalf("result: %+v", r)
		}
	}
	for _, path := range []string{a, b} {
		out := model.outputs[path]
		if !out.committed || !bytes.Equal(out.data, content) {
			t.Fatalf("output %q is incomplete", path)
		}
	}
	if model.peakWrite != 1 || model.accepted != int64(2*len(content)) {
		t.Fatalf("write concurrency=%d accepted=%d", model.peakWrite, model.accepted)
	}
}

func TestMockSingleFUSEDispatchQueuesWriteBehindClose(t *testing.T) {
	// A waiting Close holds the calibrated single FUSE dispatcher, independently of application calls.
	synctest.Test(t, func(t *testing.T) {
		closeGate, closeEntered, writeCalled, writeEntered := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		m := newLTFSModel(ltfsModelConfig{dispatchers: 1, before: func(op, path string) {
			if op == "write" {
				close(writeCalled)
			}
		}, entered: func(op, path string) {
			if op == "close" {
				close(closeEntered)
				<-closeGate
			}
			if op == "write" {
				close(writeEntered)
			}
		}})
		a, b := &ltfsOutput{model: m, path: "a"}, &ltfsOutput{model: m, path: "b"}
		go func() { _ = a.Close() }()
		<-closeEntered
		go func() { _, _ = b.Write([]byte("x")) }()
		<-writeCalled
		synctest.Wait()
		select {
		case <-writeEntered:
			t.Fatal("Write executed inside a blocked single-dispatch Close")
		default:
		}
		close(closeGate)
		<-writeEntered
		synctest.Wait()
		if err := m.validate(); err != nil {
			t.Fatal(err)
		}
		m.shutdown()
	})
}

func TestPipelineLinearReadAdvancesBeforeSourceClose(t *testing.T) {
	// Tape source reads retain their order, while the prior descriptor closes independently.
	root := t.TempDir()
	a := writeSourceFile(t, root, "a", []byte("aaaa"))
	b := writeSourceFile(t, root, "b", []byte("bbbb"))
	gate, closing, reading := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := onceRelease(gate)
	t.Cleanup(release)
	var closeOnce, readOnce sync.Once
	m := newLTFSModel(ltfsModelConfig{entered: func(op, path string) {
		if op == "source-close" && path == a {
			closeOnce.Do(func() { close(closing) })
			<-gate
		}
		if op == "read" && path == b {
			readOnce.Do(func() { close(reading) })
		}
	}})
	defer m.shutdown()
	var seen atomic.Int64
	c, err := NewStream(context.Background(), func(r []Result) error { seen.Add(int64(len(r))); return nil }, WithHashPolicy(HashRead), SetFromDevice(LinearDevice(true)))
	if err != nil {
		t.Fatal(err)
	}
	c.filesystem = ltfsFilesystem{transferFilesystem: c.fs(), model: m, readPositions: map[string]int64{a: 0, b: 4}}
	if err := c.Submit(&SimpleJob{Path: a}, &SimpleJob{Path: b}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	awaitStage(t, closing, "first source Close")
	awaitStage(t, reading, "second ordered Read")
	release()
	awaitStage(t, done, "source drain")
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 2 || m.peakRead != 1 || m.seeks != 0 {
		t.Fatalf("results=%d read concurrency=%d seeks=%d", seen.Load(), m.peakRead, m.seeks)
	}
}

func TestPipelineLateCloseNoSpaceKeepsAlreadyStartedOutcome(t *testing.T) {
	// The first late Close fails only after the second Write has entered; the third must stay unstarted.
	root := t.TempDir()
	src := writeSourceFile(t, root, "source", []byte("late close"))
	a, b, d := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "d")
	secondStarted, secondGate := make(chan struct{}), make(chan struct{})
	releaseSecond := onceRelease(secondGate)
	t.Cleanup(releaseSecond)
	m := newLTFSModel(ltfsModelConfig{before: func(op, path string) {
		if op == "write" && path == b {
			close(secondStarted)
			<-secondGate
		}
	}, closeError: func(path string) error {
		if path == a {
			<-secondStarted
			return syscall.ENOSPC
		}
		return nil
	}})
	defer m.shutdown()
	results := make(chan Result, 3)
	c, err := NewStream(context.Background(), func(batch []Result) error {
		for _, r := range batch {
			results <- r
		}
		return nil
	}, WithHashPolicy(HashRead), WithResultBatch(1), SetToDevice(LinearDevice(true)))
	if err != nil {
		t.Fatal(err)
	}
	c.filesystem = ltfsFilesystem{transferFilesystem: c.fs(), model: m}
	if err := c.Submit(&SimpleJob{Path: src, Dsts: []string{a}}, &SimpleJob{Path: src, Dsts: []string{b}}, &SimpleJob{Path: src, Dsts: []string{d}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	first := <-results
	if len(first.Targets) != 1 || first.Targets[0].Path != a || !errors.Is(first.Targets[0].Err, syscall.ENOSPC) || !errors.Is(first.Targets[0].Err, ErrTargetNoSpace) {
		t.Fatalf("first result: %+v", first)
	}
	releaseSecond()
	awaitStage(t, done, "late-error drain")
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}

	// Already admitted work reports its real success; later work never allocates an output.
	outcomes := map[string]Result{a: first}
	for range 2 {
		r := <-results
		outcomes[r.Job.(*SimpleJob).Dsts[0]] = r
	}
	if outcomes[b].Err != nil || outcomes[b].Targets[0].Err != nil || !errors.Is(outcomes[d].Err, ErrTargetNoSpace) {
		t.Fatalf("outcomes: %+v", outcomes)
	}
	if m.outputs[d] != nil || !m.outputs[a].discarded || m.outputs[a].committed || !m.outputs[b].committed {
		t.Fatal("late Close publication/admission mismatch")
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineRandomTargetsWriteConcurrently(t *testing.T) {
	// Two target consumers must reach Write together even with only one active file.
	root := t.TempDir()
	src := writeSourceFile(t, root, "source", []byte("fanout"))
	gate := make(chan struct{})
	release := onceRelease(gate)
	t.Cleanup(release)
	entered := make(chan struct{}, 2)
	m := newLTFSModel(ltfsModelConfig{capture: true, before: func(op, path string) {
		if op == "write" {
			entered <- struct{}{}
			<-gate
		}
	}})
	defer m.shutdown()
	c, err := NewStream(context.Background(), func([]Result) error { return nil }, WithHashPolicy(HashRead), SetToDevice(DeviceThreads(1)))
	if err != nil {
		t.Fatal(err)
	}
	c.filesystem = ltfsFilesystem{transferFilesystem: c.fs(), model: m}
	if err := c.Submit(&SimpleJob{Path: src, Dsts: []string{filepath.Join(root, "a"), filepath.Join(root, "b")}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	awaitStage(t, entered, "first random target Write")
	awaitStage(t, entered, "second random target Write")
	release()
	awaitStage(t, done, "fanout drain")
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	if m.peakWrite != 2 {
		t.Fatalf("target concurrency=%d", m.peakWrite)
	}
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

func TestPipelineResultBatchLargerThanLifetimeLimit(t *testing.T) {
	// The lightweight result buffer must not retain file slots until the caller's large batch fills.
	root := t.TempDir()
	source := writeSourceFile(t, root, "source", nil)
	const total = transferLimit * 3
	seen := 0
	c, err := NewStream(context.Background(), func(results []Result) error { seen += len(results); return nil }, WithResultBatch(total), WithResultBuffer(1), WithResultFlushInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for range total {
		if err := c.Submit(&SimpleJob{Path: source}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if seen != total {
		t.Fatalf("results=%d want=%d", seen, total)
	}
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
		if ahead.allocated != readAheadChunks || cap(replacement.data) != batchSize {
			t.Fatalf("backing grew outside budget: allocated=%d capacity=%d", ahead.allocated, cap(replacement.data))
		}
		replacement.release()
		for _, chunk := range held {
			if chunk != nil {
				chunk.release()
			}
		}
		if ahead.used != 0 || ahead.futureUsed != 0 {
			t.Fatalf("leaked credits: used=%d future=%d", ahead.used, ahead.futureUsed)
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
