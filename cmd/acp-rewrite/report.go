package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
	"github.com/sirupsen/logrus"
)

func findJob(report *acp.Report, filePath string) (*acp.Job, bool) {
	for _, job := range report.Jobs {
		if job.FullPath == filePath {
			return job, true
		}
	}
	return nil, false
}

// loadReport reads the report accumulated by previous runs. A report that exists but cannot
// be decoded is an error: starting over with an empty history would drop every row the
// earlier runs recorded.
func loadReport(path string) (map[string]*acp.Job, []*acp.Error, error) {
	// An absent report starts an empty history without creating a document during a dry run.
	jobs := make(map[string]*acp.Job, 128)
	errors := make([]*acp.Error, 0)
	if path == "" {
		return jobs, errors, nil
	}

	// Decode one complete document before inspecting rows or changing any saved paths.
	var report acp.Report
	if err := fileio.ReadJSON(path, &report); err != nil {
		if os.IsNotExist(err) {
			return jobs, errors, nil
		}
		return nil, nil, fmt.Errorf("decode report fail, path= %q, %w", path, err)
	}

	// Validate loaded rows before they enter the accumulated history.
	for _, job := range report.Jobs {
		if job == nil {
			return nil, nil, fmt.Errorf("report contains a null file row")
		}
		if err := checkPaths(job.FullPath); err != nil {
			return nil, nil, err
		}
		if _, exists := jobs[job.FullPath]; exists {
			return nil, nil, fmt.Errorf("report repeats file %q", job.FullPath)
		}
		for _, paths := range [][]string{{job.Base}, job.Path, job.SuccessTargets} {
			if err := fileio.CheckJSONPaths(paths...); err != nil {
				return nil, nil, err
			}
		}
		for target := range job.FailTargets {
			if err := fileio.CheckJSONPaths(target); err != nil {
				return nil, nil, err
			}
		}
		jobs[job.FullPath] = job
	}
	for _, failure := range report.Errors {
		if failure == nil {
			return nil, nil, fmt.Errorf("report contains a null error row")
		}
		if err := fileio.CheckJSONPaths(failure.Src, failure.Dst); err != nil {
			return nil, nil, err
		}
	}
	errors = append(errors, report.Errors...)
	return jobs, errors, nil
}

func mergeReport(jobs map[string]*acp.Job, errors *[]*acp.Error, report *acp.Report) {
	if report == nil {
		return
	}
	for _, job := range report.Jobs {
		jobs[job.FullPath] = job
	}
	if len(report.Errors) > 0 {
		*errors = append(*errors, report.Errors...)
	}
}

// saveReport writes the accumulated report. Rows are ordered by path, so two runs of the same
// work produce the same document, and the file is replaced atomically: a crash mid-write leaves
// the previous report readable instead of a document the next run refuses to decode.
func saveReport(path string, indent bool, jobs map[string]*acp.Job, errors []*acp.Error) error {
	if path == "" {
		return nil
	}

	// Preserve deterministic full-path order in the accumulated document.
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)

	// Build and atomically publish a snapshot without changing saved rows on failure.
	report := &acp.Report{
		Jobs:   make([]*acp.Job, 0, len(jobs)),
		Errors: errors,
	}
	for _, name := range names {
		report.Jobs = append(report.Jobs, jobs[name])
	}

	// Publication owns its exclusive temporary and reports failed cleanup separately.
	return fileio.WriteJSON(path, report, indent)
}

func printDuplicates(jobs map[string]*acp.Job) {
	type dupKey struct {
		size int64
		hash string
	}
	// Group only complete, non-empty content signatures.
	dups := make(map[dupKey][]string)
	for _, job := range jobs {
		if job == nil || job.SHA256 == "" || job.Size == 0 {
			continue
		}
		key := dupKey{size: job.Size, hash: job.SHA256}
		dups[key] = append(dups[key], job.FullPath)
	}

	// Stable group order makes duplicate output comparable across resumed runs.
	keys := make([]dupKey, 0, len(dups))
	for key := range dups {
		if len(dups[key]) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].size != keys[j].size {
			return keys[i].size < keys[j].size
		}
		return keys[i].hash < keys[j].hash
	})

	// Present each matching group with stable path order.
	for _, key := range keys {
		paths := dups[key]
		sort.Strings(paths)
		logrus.Infof("duplicate size= %d sha256= %s files= %v", key.size, key.hash, paths)
	}
}
