package acp

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	mapset "github.com/deckarep/golang-set/v2"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type publicationOrder struct {
	lock    sync.Mutex
	pending map[string]chan struct{}
}

type publicationTurn struct {
	key        string
	previous   <-chan struct{}
	directTurn <-chan struct{}
	done       chan struct{}
	release    func()
}

func newPublicationOrder() *publicationOrder {
	return &publicationOrder{pending: make(map[string]chan struct{})}
}

func publicationKey(path string) string {
	// ASCII paths need only case folding; avoid Unicode transform buffers for ordinary names.
	for i := range len(path) {
		if path[i] >= utf8.RuneSelf {
			return norm.NFD.String(cases.Fold().String(norm.NFD.String(path)))
		}
	}
	return strings.ToLower(path)
}

func (p *publicationOrder) reserve(path string) publicationTurn {
	// Canonical caseless matching conservatively orders native aliases without changing their paths.
	key := publicationKey(path)

	// Only potentially conflicting paths share a dependency; finished entries do not accumulate.
	p.lock.Lock()
	defer p.lock.Unlock()
	done := make(chan struct{})
	turn := publicationTurn{key: key, previous: p.pending[key], done: done}
	p.pending[key] = done
	turn.release = func() {
		// A failed middle output must retain its predecessor's publication dependency.
		if turn.previous != nil {
			<-turn.previous
		}

		// Completion removes only its own latest ticket, keeping path history bounded.
		p.lock.Lock()
		defer p.lock.Unlock()
		close(done)
		if p.pending[key] == done {
			delete(p.pending, key)
		}
	}
	return turn
}

// ordersPublication reports whether conflicting outputs publish in dispatch order. Linear
// and single-threaded configurations keep their winners across asynchronous completion.
func (c *StreamCopyer) ordersPublication() bool {
	return c.toDevice.linear || c.toDevice.threads == 1 || c.fromDevice.linear
}

func (t *transfer) reservePublications(order *publicationOrder) {
	if t.noContent || !t.copyer.ordersPublication() {
		return
	}

	// Derive each key once and reuse its first predecessor for direct aliases within this item.
	t.publications = make([]publicationTurn, len(t.job.outputs))
	for i, spec := range t.job.outputs {
		if spec.content != nil {
			continue
		}
		turn := order.reserve(spec.path)
		if spec.direct {
			// Aliases in one file share only the prior file's device lifetime, never their own data gate.
			turn.directTurn = turn.previous
			for j := 0; j < i; j++ {
				if t.publications[j].key == turn.key {
					turn.directTurn = t.publications[j].previous
					break
				}
			}
		}
		t.publications[i] = turn
	}
}

type targetTransfer struct {
	spec    targetSpec
	output  transferOutput
	content ContentWriter
	encoder io.WriteCloser
	stored  int64
	// storedHash hashes the encoded bytes while the transfer hashes its content; storedSHA256 is
	// its sum once the encoder has closed.
	storedHash   hash.Hash
	storedSHA256 []byte
	chunks       chan *chunkBuffer
	err          error
	publication  publicationTurn
}

// storedWriter counts, and for an encoded output hashes, the bytes a native output stores,
// beneath any item encoder.
type storedWriter struct{ target *targetTransfer }

func (w storedWriter) Write(data []byte) (int, error) {
	n, err := w.target.output.Write(data)
	w.target.stored += int64(n)
	if w.target.storedHash != nil {
		_, _ = w.target.storedHash.Write(data[:n])
	}
	return n, err
}

// startEncoder wraps a created native output in the item's encoder, hashing what it stores when
// the transfer hashes its content.
func (target *targetTransfer) startEncoder(c *StreamCopyer, encode func(io.Writer) (io.WriteCloser, error), hashed bool) error {
	if hashed {
		target.storedHash = sha256Pool.Get().(hash.Hash)
		target.storedHash.Reset()
	}
	var err error
	panicErr := c.protectCall("item target encoder", func() { target.encoder, err = encode(storedWriter{target}) })
	err = errors.Join(err, panicErr)
	if err == nil && target.encoder == nil {
		err = fmt.Errorf("target encoder returned no writer")
	}
	if err != nil {
		return fmt.Errorf("start target encoder failed, %w", err)
	}
	return nil
}

// closeEncoder flushes the encoded tail once, including after a failed write.
func (target *targetTransfer) closeEncoder(c *StreamCopyer) error {
	var err error
	panicErr := c.protectCall("item target encoder Close", func() { err = target.encoder.Close() })
	err = errors.Join(err, panicErr)
	if target.storedHash != nil {
		// Close returns only after every write into the target finished, so the hash is idle.
		if err == nil {
			target.storedSHA256 = target.storedHash.Sum(nil)
		}
		sha256Pool.Put(target.storedHash)
		target.storedHash = nil
	}
	if err != nil {
		return fmt.Errorf("close target encoder failed, %w", err)
	}
	return nil
}

// contentWrite is the state of one item's data stage: the targets it feeds, their random-target
// data consumers, and whether the item stopped its own read early so that stop is no failure.
type contentWrite struct {
	targets []*targetTransfer
	data    sync.WaitGroup
	aborted bool
}

func (t *transfer) writeContent(cntr *counter, exhausted mapset.Set[string]) {
	// Data consumers settle before ownership moves to independent completion workers.
	w := &contentWrite{}
	defer func() {
		var failure error
		if value := recover(); value != nil {
			failure = t.copyer.fatalPanic("target data", value)
		}
		t.settleTargets(w, failure, exhausted)
	}()

	// Source endpoint Open must settle before any output allocation or endpoint header emission.
	if t.sourceReady != nil {
		<-t.sourceReady
	}
	if t.job.itemError != nil {
		return
	}

	// Prepare each independent outcome before transferring output ownership to its consumer.
	for i, spec := range t.job.outputs {
		if t.openOutput(w, i, spec, exhausted) {
			break
		}
	}

	// Failed consumers keep draining; only a native item with no content consumer stops reading
	// early. A content source always reads its member to the end, keeping its stream's boundary.
	if len(w.targets) == 0 && t.hashes == nil && t.job.contentSource == nil {
		w.aborted = true
		t.abort()
	}
	t.distribute(w, cntr)
}

// settleTargets runs when the data stage ends, normally or by panic: it closes the consumers,
// waits for the read, closes each encoder and hands every target to its completion worker.
func (t *transfer) settleTargets(w *contentWrite, failure error, exhausted mapset.Set[string]) {
	c := t.copyer
	for _, target := range w.targets {
		if target.chunks != nil {
			close(target.chunks)
		}
	}
	w.data.Wait()
	<-t.readDone
	for _, target := range w.targets {
		target.err = errors.Join(target.err, t.readErr, failure)
		// The encoded tail belongs to this file's data writing, before a linear writer moves on.
		if target.encoder != nil {
			if err := target.closeEncoder(t.copyer); err != nil {
				target.err = errors.Join(target.err, err)
				c.endLinearTarget(err)
			}
		}
		t.targets.Add(1)
		go c.wrap(func() {
			defer t.targets.Done()
			t.finishTarget(target, exhausted)
		})
	}
	// With no target to carry it, a read failure is the item's own; a drained member reports it
	// too, while an item that aborted its read keeps the stop out of its result.
	if t.readErr != nil && len(w.targets) == 0 && (t.hashes != nil || !w.aborted) {
		t.job.itemError = t.readErr
	}
}

// openOutput opens output i and registers its target. It reports true when the item must stop
// opening further outputs.
func (t *transfer) openOutput(w *contentWrite, i int, spec targetSpec, exhausted mapset.Set[string]) bool {
	c := t.copyer

	// A direct device becomes visible during Write and may require the preceding handle to close.
	if i < len(t.publications) && !t.wait(t.publications[i].directTurn) {
		t.job.fail(spec.name, fmt.Errorf("direct target stopped by pipeline failure"))
		return false
	}
	// A preceding Close may have exhausted this device while its handle was still owned.
	if exhausted.Contains(spec.device) {
		t.job.fail(spec.name, ErrTargetNoSpace)
		return false
	}

	if spec.content != nil {
		return t.openContentOutput(w, i, spec, exhausted)
	}
	output, err := c.fs().Create(t.job, spec)
	if err != nil {
		c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
		return false
	}
	target := &targetTransfer{spec: spec, output: output}
	if t.job.encode != nil {
		// A refused encoder leaves its output unpublished, like any other creation failure.
		if err := target.startEncoder(t.copyer, t.job.encode, t.hashes != nil); err != nil {
			c.targetFailed(t.job.baseJob, spec.name, spec.device, errors.Join(err, output.Discard()), exhausted)
			return false
		}
	}
	t.addTarget(w, i, target)
	return false
}

// openContentOutput opens a content target. Its failure stops the endpoint and every later
// output, unless the writer refused the member before emitting anything.
func (t *transfer) openContentOutput(w *contentWrite, i int, spec targetSpec, exhausted mapset.Set[string]) bool {
	c := t.copyer
	var content ContentWriter
	metadata, err := snapshotContentMetadata(t.job.metadata)
	if err == nil {
		panicErr := t.copyer.protectCall("ContentTarget.Open", func() { content, err = spec.content.Open(c.ctx, metadata) })
		err = errors.Join(err, panicErr)
	}
	if err == nil && content == nil {
		err = fmt.Errorf("content target returned no writer")
	}
	if err == nil {
		t.addTarget(w, i, &targetTransfer{spec: spec, content: content})
		return false
	}

	// A writer that refused the member before emitting anything fails only this target.
	if content == nil && errors.Is(err, ErrMemberSettled) {
		c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
		return false
	}
	if content != nil {
		// Even an errored Open can return an owned handle; Finish still waits for Close.
		w.targets = append(w.targets, &targetTransfer{spec: spec, content: content, err: err})
	} else {
		c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
	}
	for _, remaining := range t.job.outputs[i+1:] {
		t.job.fail(remaining.name, err)
	}
	t.stopEndpoint(err)
	w.aborted = true
	t.abort()
	return true
}

// addTarget registers an opened target with its publication turn and, for a random target, its
// data consumer.
func (t *transfer) addTarget(w *contentWrite, i int, target *targetTransfer) {
	if i < len(t.publications) {
		target.publication = t.publications[i]
		t.publications[i] = publicationTurn{}
	}
	w.targets = append(w.targets, target)
	if t.copyer.toDevice.linear {
		return
	}
	target.chunks = make(chan *chunkBuffer, 4)
	w.data.Add(1)
	go t.copyer.wrap(func() {
		defer w.data.Done()
		t.writeTargetData(target)
	})
}

// distribute hands every chunk of the read to the targets until the read ends or the pipeline
// stops hard, and advances the read-ahead window by what was consumed.
func (t *transfer) distribute(w *contentWrite, cntr *counter) {
	c := t.copyer
	var consumed int64
	for chunk := range t.chunks {
		continued := func() bool {
			defer chunk.release()
			if len(w.targets) == 0 && t.hashes == nil {
				// A drained member still advances the read-ahead window, or its reader waits forever.
				consumed += int64(len(chunk.data))
				t.ahead.advance(t.job.order, t.start+consumed)
				return true
			}
			for _, target := range w.targets {
				if c.toDevice.linear {
					// All linear outputs share this one application data writer.
					if target.err == nil {
						if err := writeChunk(target.writer(), chunk.retain()); err != nil {
							target.err = err
							c.endLinearTarget(err)
							if target.content != nil {
								t.stopEndpoint(err)
								t.abort()
							}
						}
					}
					continue
				}
				reference := chunk.retain()
				select {
				case target.chunks <- reference:
				case <-c.hardStop:
					reference.release()
					return false
				}
			}
			consumed += int64(len(chunk.data))
			atomic.AddInt64(&cntr.bytes, int64(len(chunk.data)))
			t.ahead.advance(t.job.order, t.start+consumed)
			return true
		}()
		if !continued {
			return
		}
	}
}

func (t *transfer) writeTargetData(target *targetTransfer) {
	// Each random output owns its errors and drains every reference after failure or panic.
	defer func() {
		if value := recover(); value != nil {
			target.err = errors.Join(target.err, t.copyer.fatalPanic("target data consumer", value))
		}
		for chunk := range target.chunks {
			chunk.release()
		}
	}()
	for chunk := range target.chunks {
		if target.err != nil {
			chunk.release()
			continue
		}
		if err := writeChunk(target.writer(), chunk); err != nil {
			target.err = err
			t.copyer.endLinearTarget(err)
			if target.content != nil {
				t.stopEndpoint(err)
				t.abort()
			}
		}
	}
}

func (t *transfer) finishTarget(target *targetTransfer, exhausted mapset.Set[string]) {
	// Stream Finish owns its complete lifecycle and never invokes native filesystem operations.
	if target.content != nil {
		t.finishContentTarget(target)
		return
	}

	// Publication release survives a secondary panic while cleanup reports its outcome.
	if target.publication.release != nil {
		defer target.publication.release()
	}

	// One completion owner closes/discards the output and reports its terminal outcome.
	c, out := t.copyer, target.output
	defer func() {
		if value := recover(); value != nil {
			target.err = errors.Join(target.err, t.copyer.fatalPanic("target completion", value))
		}
		cleanup := t.copyer.protectCall("discard target", func() { target.err = errors.Join(target.err, out.Discard()) })
		if cleanup != nil {
			target.err = errors.Join(target.err, cleanup)
			c.setError(cleanup)
			c.stopHard()
		}
		if target.err != nil {
			c.targetFailed(t.job.baseJob, target.spec.name, target.spec.device, target.err, exhausted)
		} else {
			storedHash := target.storedSHA256
			if target.content == nil && t.job.encode == nil && t.hashes != nil && !t.noContent {
				// A plaintext native target stores the bytes the content hash describes.
				storedHash = t.digest
			}
			t.job.success(target.spec.name, target.stored, storedHash)
		}
	}()

	// Linear Close is independent of hash and the next data producer, and its error is authoritative.
	// It ends the linear target at once; every other failure reaches targetFailed, which ends it.
	if c.toDevice.linear {
		if err := out.Close(); err != nil {
			target.err = errors.Join(target.err, fmt.Errorf("close target failed, %w", err))
			c.endLinearTarget(target.err)
		}
	}
	if target.err != nil {
		return
	}
	t.publishHash()
	if t.hashErr != nil {
		target.err = t.hashErr
		return
	}
	select {
	case <-c.hardStop:
		target.err = fmt.Errorf("target stopped by pipeline failure")
		return
	default:
	}

	// A stream source must settle padding/cursor validation before publishing any native output.
	if t.job.contentSource != nil {
		<-t.sourceDone
		if t.sourceErr != nil {
			target.err = fmt.Errorf("close content source failed, %w", t.sourceErr)
			return
		}
	}

	// A decoder Close can still reject the plaintext, so it settles before any native output publishes.
	if t.job.decode != nil {
		<-t.sourceDone
		if t.sourceErr != nil {
			target.err = fmt.Errorf("close decoded source failed, %w", t.sourceErr)
			return
		}
	}

	// Managed metadata precedes restrictive permissions; ordinary random targets retain their sync order.
	if !target.spec.direct {
		if err := out.Cache(t.job.baseJob, target.stored, target.storedSHA256); err != nil {
			target.err = fmt.Errorf("write target signature failed, %w", err)
			return
		}
		if err := out.Restore(t.job.stat); err != nil {
			target.err = fmt.Errorf("restore target metadata failed, %w", err)
			return
		}
	}
	if !c.toDevice.linear {
		if err := out.Sync(); err != nil {
			target.err = fmt.Errorf("sync target failed, %w", err)
			return
		}
	}

	// Rewrite cannot replace the source path before its independent cache and source Close have settled.
	if target.spec.output != nil {
		<-t.sourceCacheDone
		if t.sourceErr != nil {
			target.err = fmt.Errorf("close rewrite source failed, %w", t.sourceErr)
			return
		}
	}
	if !t.wait(target.publication.previous) {
		target.err = fmt.Errorf("publication stopped by pipeline failure")
		return
	}
	if err := out.Commit(c.overwrite); err != nil {
		target.err = fmt.Errorf("commit target failed, %w", err)
	}
}

func (target *targetTransfer) writer() io.Writer {
	switch {
	case target.content != nil:
		return target.content
	case target.encoder != nil:
		return target.encoder
	}
	return storedWriter{target}
}

func (t *transfer) finishContentTarget(target *targetTransfer) {
	// Both source validation and hashing settle before one terminal Finish receives their causes.
	t.publishHash()
	<-t.sourceDone
	target.err = errors.Join(target.err, t.sourceErr, t.hashErr)
	// The cause reaches Finish behind its own node, so a marker Finish merely passes on from it is
	// told apart from a marker Finish adds itself.
	var cause error
	if target.err != nil {
		cause = &finishCause{err: target.err}
	}
	var finishErr error
	panicErr := t.copyer.protectCall("ContentWriter.Finish", func() {
		finishErr = target.content.Finish(cause)
	})
	target.err = errors.Join(target.err, finishErr, panicErr)

	// A broken member stops the ordered stream, preserving each original failure cause, unless the
	// writer's own Finish settled it at a usable boundary. A source that settled its own failure
	// says nothing about the target stream, and a panicking Finish settles nothing.
	if target.err != nil {
		if panicErr != nil || !settledByWriter(finishErr, cause) {
			t.stopEndpoint(target.err)
		}
		t.job.fail(target.spec.name, target.err)
		return
	}
	t.job.success(target.spec.name, 0, nil)
}

// finishCause carries a failed member's cause into ContentWriter.Finish as a node of its own. It
// keeps errors.Is and errors.As working through the cause.
type finishCause struct{ err error }

func (c *finishCause) Error() string { return c.err.Error() }
func (c *finishCause) Unwrap() error { return c.err }

// settledByWriter reports whether a Finish error wraps ErrMemberSettled outside the cause it was
// given, following the same Is and Unwrap rules as errors.Is. Returning, wrapping or joining the
// cause therefore never settles a member, even when the cause carries a source's own marker.
func settledByWriter(err, cause error) bool {
	if err == nil || err == cause {
		return false
	}
	if err == ErrMemberSettled {
		return true
	}
	if matcher, ok := err.(interface{ Is(error) bool }); ok && matcher.Is(ErrMemberSettled) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() error }:
		return settledByWriter(wrapped.Unwrap(), cause)
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			if settledByWriter(child, cause) {
				return true
			}
		}
	}
	return false
}
