package acp

import (
	"encoding/json"
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

		errorsCopyed := make([]*Error, 0, len(errors))
		errorsCopyed = append(errorsCopyed, errors...)

		return &Report{
			Jobs:   jobsCopyed,
			Errors: errorsCopyed,
		}
	}
	return handler, getter
}

// Report is the JSON document of one run: one row per accepted item plus the pipeline-level
// errors. The standard library encodes and decodes it as-is, so a consumer needs no ACP coder
// to read what ACP wrote.
type Report struct {
	Jobs   []*Job   `json:"files,omitempty"`
	Errors []*Error `json:"errors,omitempty"`
}

// ToJSONString renders the report. Indentation is two spaces, because a JSON encoder accepts
// nothing else and a report writer must never panic while it is producing a document.
func (r *Report) ToJSONString(indent bool) string {
	if indent {
		buf, _ := json.MarshalIndent(r, "", "  ")
		return string(buf)
	}

	buf, _ := json.Marshal(r)
	return string(buf)
}
