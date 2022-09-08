package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func (c *Copyer) Report() *Report {
	jobs, errs := c.getJobs(), c.getErrors()

	files := make([]*File, 0, len(jobs))
	for _, job := range jobs {
		files = append(files, job.report())
	}

	return &Report{
		Files:  files,
		Errors: errs,
	}
}

var (
	_ = error(new(Error))
	_ = json.Marshaler(new(Error))
	_ = json.Unmarshaler(new(Error))
)

type Error struct {
	Path string `json:"path,omitempty"`
	Err  error  `json:"error,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("[%s]: %s", e.Path, e.Err)
}

func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"path": e.Path, "error": e.Err.Error()})
}

func (e *Error) UnmarshalJSON(buf []byte) error {
	m := make(map[string]string, 2)
	if err := json.Unmarshal(buf, &m); err != nil {
		return err
	}

	e.Path, e.Err = m["path"], fmt.Errorf(m["error"])
	return nil
}

type File struct {
	Source       string `json:"source"`
	RelativePath string `json:"relative_path"`

	Status         string            `json:"status"`
	SuccessTargets []string          `json:"success_target"`
	FailTargets    map[string]string `json:"fail_target"`

	Size      int64       `json:"size"`
	Mode      os.FileMode `json:"mode"`
	ModTime   time.Time   `json:"mod_time"`
	WriteTime time.Time   `json:"write_time"`
	SHA256    string      `json:"sha256"`
}

type Report struct {
	Files  []*File  `json:"files,omitempty"`
	Errors []*Error `json:"errors,omitempty"`
}
