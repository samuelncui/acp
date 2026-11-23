package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// ordersPublication reports whether conflicting outputs publish in dispatch order. Formerly
// serialized configurations keep their winners across asynchronous completion.
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
	spec        targetSpec
	output      transferOutput
	content     ContentWriter
	encoder     io.WriteCloser
	stored      int64
	chunks      chan *chunkBuffer
	err         error
	publication publicationTurn
}

// storedWriter counts the bytes a native output stores, beneath any item encoder.
type storedWriter struct{ target *targetTransfer }

func (w storedWriter) Write(data []byte) (int, error) {
	n, err := w.target.output.Write(data)
	w.target.stored += int64(n)
	return n, err
}

// startEncoder wraps a created native output in the item's encoder.
func (target *targetTransfer) startEncoder(encode func(io.Writer) (io.WriteCloser, error)) error {
	var err error
	panicErr := protectCall("item target encoder", func() { target.encoder, err = encode(storedWriter{target}) })
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
func (target *targetTransfer) closeEncoder() error {
	var err error
	panicErr := protectCall("item target encoder Close", func() { err = target.encoder.Close() })
	if err = errors.Join(err, panicErr); err != nil {
		return fmt.Errorf("close target encoder failed, %w", err)
	}
	return nil
}

func (t *transfer) writeContent(cntr *counter, exhausted mapset.Set[string]) {
	// Data consumers settle before ownership moves to independent completion workers.
	c := t.copyer
	var targets []*targetTransfer
	var data sync.WaitGroup
	// aborted records that this item stopped its own read early, so the read's stop is no failure.
	var aborted bool
	defer func() {
		var failure error
		if value := recover(); value != nil {
			failure = t.panicFailure("target data", value)
		}
		for _, target := range targets {
			if target.chunks != nil {
				close(target.chunks)
			}
		}
		data.Wait()
		<-t.readDone
		for _, target := range targets {
			target.err = errors.Join(target.err, t.readErr, failure)
			// The encoded tail belongs to this file's data writing, before a linear writer moves on.
			if target.encoder != nil {
				if err := target.closeEncoder(); err != nil {
					target.err = errors.Join(target.err, err)
					c.endLinearTarget(err)
				}
			}
			t.targets.Add(1)
			go c.wrap(context.Background(), func() {
				defer t.targets.Done()
				t.finishTarget(target, exhausted)
			})
		}
		// With no target to carry it, a read failure is the item's own; a drained member reports it
		// too, while an item that aborted its read keeps the stop out of its result.
		if t.readErr != nil && len(targets) == 0 && (t.hashes != nil || !aborted) {
			t.job.itemError = t.readErr
		}
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
		// A direct device becomes visible during Write and may require the preceding handle to close.
		if i < len(t.publications) && !t.wait(t.publications[i].directTurn) {
			t.job.fail(spec.name, fmt.Errorf("direct target stopped by pipeline failure"))
			continue
		}
		// A preceding Close may have exhausted this device while its handle was still owned.
		if exhausted.Contains(spec.device) {
			t.job.fail(spec.name, ErrTargetNoSpace)
			continue
		}
		var output transferOutput
		var content ContentWriter
		var err error
		if spec.content != nil {
			metadata, metadataErr := snapshotContentMetadata(t.job.metadata)
			err = metadataErr
			if err == nil {
				panicErr := protectCall("ContentTarget.Open", func() { content, err = spec.content.Open(c.ctx, metadata) })
				err = errors.Join(err, panicErr)
			}
			if err == nil && content == nil {
				err = fmt.Errorf("content target returned no writer")
			}
		} else {
			output, err = c.fs().Create(t.job, spec)
		}
		if err != nil {
			if spec.content != nil {
				// A writer that refused the member before emitting anything fails only this target.
				if content == nil && errors.Is(err, ErrMemberSettled) {
					c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
					continue
				}
				if content != nil {
					// Even an errored Open can return an owned handle; Finish still waits for Close.
					targets = append(targets, &targetTransfer{spec: spec, content: content, err: err})
				} else {
					c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
				}
				for _, remaining := range t.job.outputs[i+1:] {
					t.job.fail(remaining.name, err)
				}
				t.stopEndpoint(err)
				aborted = true
				t.abort()
				break
			}
			c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
			continue
		}
		target := &targetTransfer{spec: spec, output: output, content: content}
		if spec.content == nil && t.job.encode != nil {
			// A refused encoder leaves its output unpublished, like any other creation failure.
			if err := target.startEncoder(t.job.encode); err != nil {
				c.targetFailed(t.job.baseJob, spec.name, spec.device, errors.Join(err, output.Discard()), exhausted)
				continue
			}
		}
		if i < len(t.publications) {
			target.publication = t.publications[i]
			t.publications[i] = publicationTurn{}
		}
		targets = append(targets, target)
		if !c.toDevice.linear {
			target.chunks = make(chan *chunkBuffer, 4)
			data.Add(1)
			go c.wrap(context.Background(), func() {
				defer data.Done()
				t.writeTargetData(target)
			})
		}
	}

	// Failed consumers keep draining; only a native item with no content consumer stops reading
	// early. A content source always reads its member to the end, keeping its stream's boundary.
	if len(targets) == 0 && t.hashes == nil && t.job.contentSource == nil {
		aborted = true
		t.abort()
	}
	var consumed int64
	for chunk := range t.chunks {
		continued := func() bool {
			defer chunk.release()
			if len(targets) == 0 && t.hashes == nil {
				// A drained member still advances the read-ahead window, or its reader waits forever.
				consumed += int64(len(chunk.data))
				t.ahead.advance(t.job.order, t.start+consumed)
				return true
			}
			for _, target := range targets {
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
			target.err = errors.Join(target.err, t.panicFailure("target data consumer", value))
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
			target.err = errors.Join(target.err, t.panicFailure("target completion", value))
		}
		cleanup := protectCall("discard target", func() { target.err = errors.Join(target.err, out.Discard()) })
		if cleanup != nil {
			target.err = errors.Join(target.err, cleanup)
			c.setError(cleanup)
			c.stopHard()
		}
		if target.err != nil {
			c.targetFailed(t.job.baseJob, target.spec.name, target.spec.device, target.err, exhausted)
		} else {
			t.job.success(target.spec.name, target.stored)
		}
	}()

	// Linear Close is independent of hash and the next data producer, and its error is authoritative.
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
		if err := out.Cache(t.job.baseJob, c.toDevice.linear, target.stored); err != nil {
			target.err = fmt.Errorf("write target signature failed, %w", err)
			c.endLinearTarget(target.err)
			return
		}
		if err := out.Restore(t.job.stat); err != nil {
			target.err = fmt.Errorf("restore target metadata failed, %w", err)
			c.endLinearTarget(target.err)
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
	if err := out.Commit(c.createFlag&os.O_TRUNC != 0); err != nil {
		target.err = fmt.Errorf("commit target failed, %w", err)
		c.endLinearTarget(target.err)
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
	panicErr := protectCall("ContentWriter.Finish", func() {
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
	t.job.success(target.spec.name, 0)
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
