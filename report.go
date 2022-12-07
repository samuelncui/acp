package acp

import (
	"path"
	"sync"
)

type ReportGetter func() *Report

func NewReportGetter() (EventHandler, ReportGetter) {
	var lock sync.Mutex
	jobs := make(map[string]*Job, 8)
	errors := make([]*Error, 0)

	handler := func(ev Event) {
		switch e := ev.(type) {
		case *EventUpdateJob:
			lock.Lock()
			defer lock.Unlock()

			key := path.Join(e.Job.Path...)
			jobs[key] = e.Job
		case *EventReportError:
			lock.Lock()
			defer lock.Unlock()

			errors = append(errors, e.Error)
		}
	}
	getter := func() *Report {
		lock.Lock()
		defer lock.Unlock()

		jobsCopyed := make([]*Job, 0, len(jobs))
		for _, j := range jobs {
			jobsCopyed = append(jobsCopyed, j)
		}
		errorsCopyed := make([]*Error, 0, len(jobs))
		for _, e := range errors {
			errorsCopyed = append(errorsCopyed, e)
		}

		return &Report{
			Jobs:   jobsCopyed,
			Errors: errorsCopyed,
		}
	}
	return handler, getter
}

type Report struct {
	Jobs   []*Job   `json:"files,omitempty"`
	Errors []*Error `json:"errors,omitempty"`
}
