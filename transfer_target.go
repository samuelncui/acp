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
	chunks      chan *chunkBuffer
	err         error
	publication publicationTurn
}

func (t *transfer) writeContent(cntr *counter, exhausted mapset.Set[string]) {
	// Data consumers settle before ownership moves to independent completion workers.
	c := t.copyer
	var targets []*targetTransfer
	var data sync.WaitGroup
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
			t.targets.Add(1)
			go c.wrap(context.Background(), func() {
				defer t.targets.Done()
				t.finishTarget(target, exhausted)
			})
		}
		if t.readErr != nil && len(targets) == 0 && t.hashes != nil {
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
				t.abort()
				break
			}
			c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
			continue
		}
		var turn publicationTurn
		if i < len(t.publications) {
			turn = t.publications[i]
		}
		target := &targetTransfer{spec: spec, output: output, content: content, publication: turn}
		targets = append(targets, target)
		if i < len(t.publications) {
			t.publications[i] = publicationTurn{}
		}
		if !c.toDevice.linear {
			target.chunks = make(chan *chunkBuffer, 4)
			data.Add(1)
			go c.wrap(context.Background(), func() {
				defer data.Done()
				t.writeTargetData(target)
			})
		}
	}

	// Failed consumers keep draining; only an item with no content consumer stops reading early.
	if len(targets) == 0 && t.hashes == nil {
		t.abort()
	}
	var consumed int64
	for chunk := range t.chunks {
		continued := func() bool {
			defer chunk.release()
			if len(targets) == 0 && t.hashes == nil {
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
			t.job.success(target.spec.name)
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

	// Managed metadata precedes restrictive permissions; ordinary random targets retain their sync order.
	if !target.spec.direct {
		out.Cache(t.job.baseJob, c.toDevice.linear)
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
	if target.content != nil {
		return target.content
	}
	return target.output
}

func (t *transfer) finishContentTarget(target *targetTransfer) {
	// Both source validation and hashing settle before one terminal Finish receives their causes.
	t.publishHash()
	<-t.sourceDone
	target.err = errors.Join(target.err, t.sourceErr, t.hashErr)
	panicErr := protectCall("ContentWriter.Finish", func() {
		target.err = errors.Join(target.err, target.content.Finish(target.err))
	})
	target.err = errors.Join(target.err, panicErr)

	// A broken member stops the ordered stream, preserving each original failure cause.
	if target.err != nil {
		t.stopEndpoint(target.err)
		t.job.fail(target.spec.name, target.err)
		return
	}
	t.job.success(target.spec.name)
}
