package acp

// EventHandler observes the events of one run. ACP calls it from one goroutine per
// registration and never concurrently, so a handler may keep unguarded state between events.
// WithEventHandler is last-wins, so a run has at most one handler.
type EventHandler func(Event)

type Event interface {
	iEvent()
}

type EventUpdateCount struct {
	Bytes, Files int64
	Finished     bool
}

func (*EventUpdateCount) iEvent() {}

type EventUpdateProgress struct {
	Bytes, Files int64
	Finished     bool
}

func (*EventUpdateProgress) iEvent() {}

type EventUpdateJob struct {
	Job *Job
}

func (*EventUpdateJob) iEvent() {}

type EventReportError struct {
	Error *Error
}

func (*EventReportError) iEvent() {}

// EventSignatureCacheSummary reports aggregate cache behavior for one Copyer.
type EventSignatureCacheSummary struct {
	Summary SignatureCacheSummary
}

func (*EventSignatureCacheSummary) iEvent() {}

type EventFinished struct{}

func (*EventFinished) iEvent() {}
