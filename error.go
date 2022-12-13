package acp

import (
	"encoding/json"
	"fmt"
)

var (
	_ = error(new(Error))
	_ = json.Marshaler(new(Error))
	_ = json.Unmarshaler(new(Error))
)

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

func (e *Error) Error() string {
	return fmt.Sprintf("[%s => %s]: %s", e.Src, e.Dst, e.Err)
}

func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(&jsonError{Src: e.Src, Dst: e.Dst, Err: e.Err.Error()})
}

func (e *Error) UnmarshalJSON(buf []byte) error {
	m := new(jsonError)
	if err := json.Unmarshal(buf, &m); err != nil {
		return err
	}

	e.Src, e.Dst, e.Err = m.Src, m.Dst, fmt.Errorf(m.Err)
	return nil
}
