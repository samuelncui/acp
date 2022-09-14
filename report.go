package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func (c *Copyer) Report() *Report {
	jobs, nss := c.getJobsAndNoSpaceSource()
	errs := c.getErrors()

	files := make([]*File, 0, len(jobs))
	for _, job := range jobs {
		files = append(files, job.report())
	}

	noSpaceSources := make([]*FilePath, 0, len(nss))
	for _, s := range nss {
		if len(noSpaceSources) == 0 {
			noSpaceSources = append(noSpaceSources, &FilePath{Base: s.base, RelativePaths: []string{s.relativePath}})
			continue
		}

		if last := noSpaceSources[len(noSpaceSources)-1]; last.Base == s.base {
			last.RelativePaths = append(last.RelativePaths, s.relativePath)
			continue
		}

		noSpaceSources = append(noSpaceSources, &FilePath{Base: s.base, RelativePaths: []string{s.relativePath}})
	}

	return &Report{
		Files:          files,
		NoSpaceSources: noSpaceSources,
		Errors:         errs,
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
	SuccessTargets []string          `json:"success_target,omitempty"`
	FailTargets    map[string]string `json:"fail_target,omitempty"`

	Size      int64       `json:"size"`
	Mode      os.FileMode `json:"mode"`
	ModTime   time.Time   `json:"mod_time"`
	WriteTime time.Time   `json:"write_time"`
	SHA256    string      `json:"sha256"`
}

type FilePath struct {
	Base          string   `json:"base,omitempty"`
	RelativePaths []string `json:"relative_paths,omitempty"`
}

type Report struct {
	Files          []*File     `json:"files,omitempty"`
	NoSpaceSources []*FilePath `json:"no_space_sources,omitempty"`
	Errors         []*Error    `json:"errors,omitempty"`
}
