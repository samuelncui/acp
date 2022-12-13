package acp

import (
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
