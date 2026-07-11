package acp

import (
	"errors"
	"syscall"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/modern-go/reflect2"
	"github.com/sirupsen/logrus"
)

func TestErrorJSONMarshal(t *testing.T) {
	m := map[string]error{}
	m["test"] = syscall.EROFS

	var innerNilErr *Error
	m["test-nil"] = innerNilErr

	var err error
	logrus.Infof("get error type %s", spew.Sdump(reflect2.TypeOfPtr(&err).Elem()))

	buf, _ := reportJSON.Marshal(m)
	logrus.Infof("get json %s", buf)
}

func TestReportJSONErrorRoundTrip(t *testing.T) {
	const message = "copy 100% failed"
	want := &Report{
		Jobs: []*Job{{
			FailTargets: map[string]error{"dst": errors.New(message)},
		}},
		Errors: []*Error{{Src: "src", Dst: "dst", Err: errors.New(message)}},
	}

	buf, err := reportJSON.Marshal(want)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	var got Report
	if err := reportJSON.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(got.Errors) != 1 || got.Errors[0].Err.Error() != message {
		t.Fatalf("report errors = %+v", got.Errors)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].FailTargets["dst"].Error() != message {
		t.Fatalf("fail targets = %+v", got.Jobs)
	}
}
