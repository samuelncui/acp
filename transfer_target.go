package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	mapset "github.com/deckarep/golang-set/v2"
)

type publicationOrder struct {
	lock    sync.Mutex
	pending map[string]chan struct{}
}

type publicationTurn struct {
	previous   <-chan struct{}
	directTurn <-chan struct{}
	done       chan struct{}
	release    func()
}

func newPublicationOrder() *publicationOrder {
	return &publicationOrder{pending: make(map[string]chan struct{})}
}

func (p *publicationOrder) reserve(path string) publicationTurn {
	// Only conflicting paths share a completion dependency; finished entries do not accumulate.
	p.lock.Lock()
	defer p.lock.Unlock()
	done := make(chan struct{})
	turn := publicationTurn{previous: p.pending[path], done: done}
	p.pending[path] = done
	turn.release = func() {
		// A failed middle output must retain its predecessor's publication dependency.
		if turn.previous != nil {
			<-turn.previous
		}

		// Completion removes only its own latest ticket, keeping path history bounded.
		p.lock.Lock()
		defer p.lock.Unlock()
		close(done)
		if p.pending[path] == done {
			delete(p.pending, path)
		}
	}
	return turn
}

func (t *transfer) reservePublications(order *publicationOrder) {
	// Formerly serialized files keep their conflicting-output winners across asynchronous completion.
	c := t.copyer
	if t.noContent || (!c.toDevice.linear && c.toDevice.threads != 1 && !c.fromDevice.linear) {
		return
	}
	t.publications = make([]publicationTurn, len(t.job.outputs))
	for i, spec := range t.job.outputs {
		turn := order.reserve(spec.path)
		if spec.direct {
			// Aliases in one file share only the prior file's device lifetime, never their own data gate.
			turn.directTurn = turn.previous
			for j := 0; j < i; j++ {
				if t.job.outputs[j].path == spec.path {
					turn.directTurn = t.publications[j].directTurn
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
		output, err := c.fs().Create(t.job, spec)
		if err != nil {
			c.targetFailed(t.job.baseJob, spec.name, spec.device, err, exhausted)
			continue
		}
		var turn publicationTurn
		if i < len(t.publications) {
			turn = t.publications[i]
		}
		target := &targetTransfer{spec: spec, output: output, publication: turn}
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
						if err := writeChunk(target.output, chunk.retain()); err != nil {
							target.err = err
							c.endLinearTarget(err)
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
		if err := writeChunk(target.output, chunk); err != nil {
			target.err = err
			t.copyer.endLinearTarget(err)
		}
	}
}

func (t *transfer) finishTarget(target *targetTransfer, exhausted mapset.Set[string]) {
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
