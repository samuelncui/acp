package acp

import (
	"fmt"
	"path"
	"sync"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
	"github.com/modern-go/reflect2"
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
		errorsCopyed = append(errorsCopyed, errors...)

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

func (r *Report) ToJSONString(indent bool) string {
	if indent {
		buf, _ := reportJSON.MarshalIndent(r, "", "\t")
		return string(buf)
	}

	buf, _ := reportJSON.Marshal(r)
	return string(buf)
}

var (
	reportJSON jsoniter.API
)

type errValCoder struct{}

func (*errValCoder) IsEmpty(ptr unsafe.Pointer) bool {
	val := (*error)(ptr)
	return val == nil || *val == nil || reflect2.IsNil(*val)
}

func (*errValCoder) Encode(ptr unsafe.Pointer, stream *jsoniter.Stream) {
	val := (*error)(ptr)
	if val == nil || *val == nil {
		stream.WriteNil()
		return
	}

	stream.WriteString((*val).Error())
}

func (*errValCoder) Decode(ptr unsafe.Pointer, iter *jsoniter.Iterator) {
	val := (*error)(ptr)
	*val = fmt.Errorf(iter.ReadString())
}

var (
	errorType2 reflect2.Type
)

type reportJSONExtension struct {
	jsoniter.DummyExtension
}

func (*reportJSONExtension) CreateDecoder(typ reflect2.Type) jsoniter.ValDecoder {
	if typ.Implements(errorType2) {
		return &errValCoder{}
	}
	return nil
}

func (*reportJSONExtension) CreateEncoder(typ reflect2.Type) jsoniter.ValEncoder {
	if typ.Implements(errorType2) {
		return &errValCoder{}
	}
	return nil
}

func init() {
	reportJSON = jsoniter.Config{
		EscapeHTML:             true,
		SortMapKeys:            true,
		ValidateJsonRawMessage: true,
	}.Froze()

	var emptyErr error
	errorType2 = reflect2.TypeOfPtr(&emptyErr).Elem()
	reportJSON.RegisterExtension(&reportJSONExtension{})
}
