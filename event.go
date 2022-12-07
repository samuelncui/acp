package acp

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

type EventFinished struct{}

func (*EventFinished) iEvent() {}
