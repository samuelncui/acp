package acp

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

type wildcardJob struct {
	src []*source
	dst []string
}

func (job *wildcardJob) check() error {
	filteredDst := make([]string, 0, len(job.dst))
	for _, p := range job.dst {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p[len(p)-1] != '/' {
			p = p + "/"
		}

		dstStat, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("check dst path '%s', %w", p, err)
		}
		if !dstStat.IsDir() {
			return fmt.Errorf("dst path is not a dir")
		}

		filteredDst = append(filteredDst, p)
	}
	job.dst = filteredDst

	if len(job.src) == 0 {
		return fmt.Errorf("source path not found")
	}
	sort.Slice(job.src, func(i, j int) bool {
		return comparePath(job.src[i].path, job.src[j].path) < 0
	})
	for _, s := range job.src {
		src := s.src()
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("check src path '%s', %w", src, err)
		}
	}

	return nil
}

func WildcardJob(opts ...WildcardJobOption) Option {
	return func(o *option) *option {
		j := new(wildcardJob)
		for _, opt := range opts {
			j = opt(j)
		}

		if len(j.src) == 0 {
			return o
		}

		o.wildcardJobs = append(o.wildcardJobs, j)
		return o
	}
}

type WildcardJobOption func(*wildcardJob) *wildcardJob

func Source(paths ...string) WildcardJobOption {
	return func(j *wildcardJob) *wildcardJob {
		for _, p := range paths {
			p = path.Clean(p)
			if p[len(p)-1] == '/' {
				p = p[:len(p)-1]
			}

			base, name := path.Split(p)
			j.src = append(j.src, &source{base: base, path: []string{name}})
		}
		return j
	}
}

func AccurateSource(base string, paths ...[]string) WildcardJobOption {
	return func(j *wildcardJob) *wildcardJob {
		for _, path := range paths {
			j.src = append(j.src, &source{base: base, path: path})
		}
		return j
	}
}

func Target(paths ...string) WildcardJobOption {
	return func(j *wildcardJob) *wildcardJob {
		j.dst = append(j.dst, paths...)
		return j
	}
}
