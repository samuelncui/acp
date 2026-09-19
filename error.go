package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

var (
	_ = error(new(Error))
	_ = json.Marshaler(new(Error))
	_ = json.Unmarshaler(new(Error))
)

// Error is one pipeline-level failure. Its JSON form stores the message, so a report stays
// readable with encoding/json and an absent error stays absent.
type Error struct {
	Src string `json:"src,omitempty"`
	Dst string `json:"dst,omitempty"`
	Err error  `json:"error,omitempty"`
}

type jsonError struct {
	Src string `json:"src,omitempty"`
	Dst string `json:"dst,omitempty"`
	Err string `json:"error,omitempty"`
}

// isNilError reports whether an error interface holds no error at all, including the typed
// nil a nil concrete pointer produces.
func isNilError(err error) bool {
	if err == nil {
		return true
	}

	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	}
	return false
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if isNilError(e.Err) {
		return fmt.Sprintf("[%s => %s]", e.Src, e.Dst)
	}
	return fmt.Sprintf("[%s => %s]: %s", e.Src, e.Dst, e.Err)
}

// MarshalJSON encodes the message of an error. A missing error stays missing: it is omitted
// instead of being rendered from a nil value, so marshalling cannot panic on the nil errors
// UnmarshalJSON can produce.
func (e *Error) MarshalJSON() ([]byte, error) {
	if e == nil {
		return []byte("null"), nil
	}

	message := ""
	if !isNilError(e.Err) {
		message = e.Err.Error()
	}
	return json.Marshal(&jsonError{Src: e.Src, Dst: e.Dst, Err: message})
}

// UnmarshalJSON decodes the object form. An absent or empty message leaves Err nil, which is
// what a report stores for it, so a round trip neither loses an error nor invents one.
func (e *Error) UnmarshalJSON(buf []byte) error {
	if e == nil {
		return errors.New("decode acp error failed, receiver is nil")
	}

	m := new(jsonError)
	if err := json.Unmarshal(buf, &m); err != nil {
		return err
	}

	e.Src, e.Dst, e.Err = m.Src, m.Dst, nil
	if m.Err != "" {
		e.Err = errors.New(m.Err)
	}
	return nil
}
