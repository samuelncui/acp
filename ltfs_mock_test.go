package acp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ltfsModel separates the FUSE dispatcher, finite host cache and one physical tape server.
// Delays other than the measured dispatcher mode are scenario inputs, not hardware predictions.
type ltfsModelConfig struct {
	adaptive                                   bool
	controlPeriod, smoothing, correction       time.Duration
	targetFill                                 float64
	slewPerSecond                              int64
	driveBufferBytes, minimumRate, resumeBytes int64
	streamRate                                 int64
	seekCost                                   func(from, to int64) time.Duration
	dispatchers                                int // One matches the profiled LTFS -s mount; zero allows concurrent FUSE requests.
	bufferBytes                                int64
	bytesPerSecond                             int64
	capacity                                   int64 // Zero means the scenario does not inject EOM.
	startup, seek, backhitch                   time.Duration
	capture                                    bool
	before, entered                            func(operation, path string)
	service                                    func(tapeRequest) error // Deterministic physical gate/error injection for unit tests.
	closeError                                 func(path string) error
}

type tapeRequest struct {
	queued         time.Time
	accepted       chan struct{}
	driverErr      error
	path           string
	read           bool
	position, size int64
	done           chan struct{}
	err            error
}

type tapeSample struct {
	host, drive              int64
	elapsed                  time.Duration
	bytes, position, pending int64
}

type tapeExtent struct {
	path           string
	position, size int64
}

type ltfsModel struct {
	driverAccepted                                                                           int64
	matching                                                                                 speedMatching
	controls                                                                                 []speedSample
	driveRequests                                                                            []*tapeRequest
	backendDone                                                                              bool
	enqueueLock                                                                              sync.Mutex
	driveDone                                                                                chan struct{}
	drivePending, drivePeak, failedBytes                                                     int64
	finalizing                                                                               bool
	state                                                                                    string
	motions                                                                                  []tapeMotion
	config                                                                                   ltfsModelConfig
	dispatch                                                                                 chan struct{}
	queue                                                                                    chan *tapeRequest
	done                                                                                     chan struct{}
	lock                                                                                     sync.Mutex
	changed                                                                                  chan struct{}
	pending, peak, accepted, completed, position, appendPosition                             int64
	busy, overshoot                                                                          time.Duration
	seeks, backhitches, activeClose, peakClose, activeWrite, peakWrite, activeRead, peakRead int
	last                                                                                     *tapeRequest
	failure                                                                                  error
	outputs                                                                                  map[string]*ltfsOutput
	extents                                                                                  []tapeExtent
	curve                                                                                    []tapeSample
	started                                                                                  time.Time
}

type tapeMotion struct {
	elapsed time.Duration
	state   string
	rate    int64
}

func newLTFSModel(config ltfsModelConfig) *ltfsModel {
	if config.bufferBytes < 0 || config.driveBufferBytes < 0 || config.minimumRate < 0 || config.minimumRate > config.bytesPerSecond {
		panic("invalid tape-model byte limits or speeds")
	}
	// The host cache is independent of ACP's shared chunk credits.
	if config.bufferBytes == 0 {
		config.bufferBytes = 256 << 20
	}
	if config.driveBufferBytes == 0 {
		config.driveBufferBytes = 256_000_000
	}
	if config.minimumRate > 0 && config.resumeBytes == 0 {
		config.resumeBytes = config.driveBufferBytes * 6 / 10
	}
	if config.adaptive {
		if config.controlPeriod == 0 {
			config.controlPeriod = 100 * time.Millisecond
		}
		if config.smoothing == 0 {
			config.smoothing = time.Second
		}
		if config.correction == 0 {
			config.correction = 2 * time.Second
		}
		if config.targetFill == 0 {
			config.targetFill = 0.6
		}
		if config.slewPerSecond == 0 {
			config.slewPerSecond = 20_000_000
		}
	}
	if config.streamRate == 0 {
		config.streamRate = config.minimumRate
		if config.streamRate == 0 {
			config.streamRate = config.bytesPerSecond
		}
	}
	if config.streamRate < config.minimumRate || config.streamRate > config.bytesPerSecond {
		panic("invalid fixed tape speed")
	}
	if config.resumeBytes > config.driveBufferBytes {
		panic("unreachable drive restart watermark")
	}
	m := &ltfsModel{driveDone: make(chan struct{}), config: config, queue: make(chan *tapeRequest, 1024), done: make(chan struct{}), changed: make(chan struct{}), outputs: make(map[string]*ltfsOutput), started: time.Now()}
	if config.dispatchers > 0 {
		m.dispatch = make(chan struct{}, config.dispatchers)
	}
	m.matching = speedMatching{last: m.started, rate: float64(config.streamRate)}
	go m.serveDrive()
	go m.serve()
	return m
}

func (m *ltfsModel) request(operation, path string) func() {
	// Observe application entry before FUSE scheduling, so queued calls remain distinguishable.
	m.lock.Lock()
	switch operation {
	case "write":
		m.activeWrite++
		m.peakWrite = max(m.peakWrite, m.activeWrite)
	case "read":
		m.activeRead++
		m.peakRead = max(m.peakRead, m.activeRead)
	case "close":
		m.activeClose++
		m.peakClose = max(m.peakClose, m.activeClose)
	}
	m.lock.Unlock()
	if m.config.before != nil {
		m.config.before(operation, path)
	}
	if m.dispatch != nil {
		m.dispatch <- struct{}{}
	}
	if m.config.entered != nil {
		m.config.entered(operation, path)
	}

	// Physical draining never needs the FUSE permit held by a waiting Flush.
	return func() {
		if m.dispatch != nil {
			<-m.dispatch
		}
		m.lock.Lock()
		defer m.lock.Unlock()
		switch operation {
		case "write":
			m.activeWrite--
		case "read":
			m.activeRead--
		case "close":
			m.activeClose--
		}
	}
}

func (m *ltfsModel) enqueue(path string, read bool, position, size int64) (*tapeRequest, error) {
	// Serialize host acceptance separately from the ledger lock so queue backpressure cannot deadlock the server.
	m.enqueueLock.Lock()
	defer m.enqueueLock.Unlock()
	// Writes reserve finite LTFS cache space until backend WRITE accepts them into the drive.
	for {
		m.lock.Lock()
		if m.failure != nil {
			err := m.failure
			m.lock.Unlock()
			return nil, err
		}
		if read || m.pending+size <= m.config.bufferBytes {
			break
		}
		changed := m.changed
		m.lock.Unlock()
		<-changed
	}
	if !read {
		position = m.appendPosition
		m.appendPosition += size
		m.pending += size
		m.peak = max(m.peak, m.pending)
	}
	r := &tapeRequest{queued: time.Now(), path: path, read: read, position: position, size: size, done: make(chan struct{}), accepted: make(chan struct{})}
	m.accepted += size
	m.last = r
	// Queue handoff keeps request order without holding the lock needed to release cache space.
	m.lock.Unlock()
	m.queue <- r
	return r, nil
}

func (m *ltfsModel) serve() {
	// LTFS backend commands are ordered, but acceptance into a drive buffer is not tape durability.
	defer close(m.done)
	defer func() { m.lock.Lock(); m.backendDone = true; m.signal(); m.lock.Unlock() }()
	for r := range m.queue {
		m.lock.Lock()
		for !r.read && m.drivePending+r.size > m.config.driveBufferBytes && m.failure == nil {
			changed := m.changed
			m.lock.Unlock()
			<-changed
			m.lock.Lock()
		}
		if !r.read {
			m.pending -= r.size
		}
		r.driverErr = m.failure
		if r.driverErr == nil && !r.read {
			m.drivePending += r.size
			m.driverAccepted += r.size
			m.drivePeak = max(m.drivePeak, m.drivePending)
		}
		m.signal()
		m.lock.Unlock()

		// Failed command tickets still release both waiters and conservation accounting.
		if r.driverErr != nil {
			m.lock.Lock()
			m.failedBytes += r.size
			m.signal()
			r.err = r.driverErr
			close(r.done)
			m.lock.Unlock()
			close(r.accepted)
			continue
		}
		m.lock.Lock()
		m.driveRequests = append(m.driveRequests, r)
		m.signal()
		m.lock.Unlock()
		close(r.accepted)
	}
}

func (m *ltfsModel) signal() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *ltfsModel) finalize() error {
	// Join all accepted physical work, including older tickets before a later rejected command.
	m.lock.Lock()
	defer m.lock.Unlock()
	m.finalizing = true
	m.signal()
	for m.completed+m.failedBytes < m.accepted || m.state == "backhitch" {
		changed := m.changed
		m.lock.Unlock()
		<-changed
		m.lock.Lock()
	}
	return m.failure
}

func (m *ltfsModel) shutdown() { _ = m.finalize(); close(m.queue); <-m.done; <-m.driveDone }

type ltfsFilesystem struct {
	transferFilesystem
	model         *ltfsModel
	readPositions map[string]int64
}

func (f ltfsFilesystem) Open(path string, mode ReadMode, info os.FileInfo) (itemSource, error) {
	// Source bytes stay real; the adapter adds the modeled tape positioning and service boundary.
	source, err := f.transferFilesystem.Open(path, mode, info)
	if err != nil {
		return source, err
	}
	if position, ok := f.readPositions[path]; ok {
		source.reader = &ltfsReader{ReadCloser: source.reader, model: f.model, path: path, position: position}
	}
	return source, nil
}

func (f ltfsFilesystem) Create(_ *writeJob, target targetSpec) (transferOutput, error) {
	// Temp ownership is represented by this output; publication is distinct from data completion.
	release := f.model.request("create", target.path)
	defer release()
	out := &ltfsOutput{model: f.model, path: target.path}
	f.model.lock.Lock()
	f.model.outputs[target.path] = out
	f.model.lock.Unlock()
	return out, nil
}

type ltfsReader struct {
	io.ReadCloser
	model    *ltfsModel
	path     string
	position int64
}

func (r *ltfsReader) Read(data []byte) (int, error) {
	// Read service shares the physical server with every other file and direction.
	release := r.model.request("read", r.path)
	defer release()
	n, err := r.ReadCloser.Read(data)
	if n == 0 {
		return n, err
	}
	ticket, readErr := r.model.enqueue(r.path, true, r.position, int64(n))
	if readErr != nil {
		return 0, readErr
	}
	<-ticket.done
	if ticket.err != nil {
		return 0, ticket.err
	}
	r.position += int64(n)
	return n, err
}

func (r *ltfsReader) Close() error {
	release := r.model.request("source-close", r.path)
	defer release()
	return r.ReadCloser.Close()
}

type ltfsOutput struct {
	model                        *ltfsModel
	path                         string
	last                         *tapeRequest
	closed, committed, discarded bool
	closeErr                     error
	data                         []byte
	bytes                        int64
}

func (o *ltfsOutput) Write(data []byte) (int, error) {
	// Calls may overlap across files, but accepted data always enters one append-only tape queue.
	release := o.model.request("write", o.path)
	defer release()
	if o.closed {
		return 0, os.ErrClosed
	}
	n := 0
	for n < len(data) {
		size := min(int64(len(data)-n), o.model.config.bufferBytes, o.model.config.driveBufferBytes, int64(1<<20))
		ticket, err := o.model.enqueue(o.path, false, 0, size)
		if err != nil {
			return n, err
		}
		o.last = ticket
		n += int(size)
	}
	if o.model.config.capture {
		o.data = append(o.data, data...)
	}
	o.bytes += int64(n)
	return n, nil
}

func (o *ltfsOutput) flush() error {
	if o.last == nil {
		return nil
	}
	<-o.last.accepted
	return o.last.driverErr
}

func (o *ltfsOutput) Close() error {
	// Close joins backend acceptance into the drive buffer; Finalize separately joins physical media.
	if o.closed {
		return o.closeErr
	}
	release := o.model.request("close", o.path)
	defer release()
	o.closed = true
	o.closeErr = o.flush()
	if o.model.config.closeError != nil {
		o.closeErr = errors.Join(o.closeErr, o.model.config.closeError(o.path))
	}
	return o.closeErr
}

func (o *ltfsOutput) Sync() error {
	release := o.model.request("sync", o.path)
	defer release()
	return o.flush()
}

func (o *ltfsOutput) Cache(_ *baseJob, _ bool) {
	release := o.model.request("cache", o.path)
	defer release()
}

func (o *ltfsOutput) Restore(_ *stat) error {
	release := o.model.request("metadata", o.path)
	defer release()
	return nil
}

func (o *ltfsOutput) Commit(_ bool) error {
	// A failed Close cannot become a published file, even if cleanup calls Close again.
	if err := o.Close(); err != nil {
		return err
	}
	release := o.model.request("rename", o.path)
	defer release()
	o.committed = true
	return nil
}

func (o *ltfsOutput) Discard() error {
	err := o.Close()
	if !o.committed {
		o.discarded = true
	}
	return err
}

func (m *ltfsModel) validate() error {
	// The mock's own ledger must settle before its measurements can support a pipeline comparison.
	if err := m.finalize(); err != nil {
		return err
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.pending != 0 || m.drivePending != 0 || m.accepted != m.completed || m.peak > m.config.bufferBytes || m.drivePeak > m.config.driveBufferBytes {
		return fmt.Errorf("unsettled tape ledger: accepted=%d completed=%d pending=%d peak=%d", m.accepted, m.completed, m.pending, m.peak)
	}
	return nil
}
