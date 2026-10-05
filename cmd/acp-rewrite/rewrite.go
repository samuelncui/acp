package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samuelncui/acp"
	"github.com/samuelncui/acp/internal/fileio"
)

var renameFile = os.Rename

func rewriteFile(ctx context.Context, item *fileio.RewriteItem) (*acp.Report, error) {
	// One sequential handoff shares the open temporary and metadata with the copy engine.
	var result acp.Result
	count := 0
	stream, err := acp.NewStream(ctx, func(results []acp.Result) error {
		for _, next := range results {
			result = next
			count++
		}
		return nil
	}, acp.WithHashPolicy(acp.HashRead), acp.Overwrite(true))
	if err != nil {
		return nil, err
	}
	defer stream.Wait()

	// Wait ends every pipeline owner, even when submission fails, before caller cleanup.
	submitErr := stream.Submit(item)
	runErr := stream.Wait()
	if submitErr != nil {
		return nil, errors.Join(submitErr, runErr)
	}
	if count != 1 {
		return nil, errors.Join(runErr, fmt.Errorf("rewrite returned %d results, want one", count))
	}

	// Preserve the existing report coordinates and observed content facts without a shell run.
	base := filepath.VolumeName(item.Path) + string(filepath.Separator)
	job := &acp.Job{
		Base: base, Path: strings.Split(filepath.ToSlash(strings.TrimPrefix(item.Path, base)), "/"),
		FullPath: item.Path, Status: acp.JobStatusFinished,
		Size: result.Size, Mode: result.Mode, ModTime: result.ModTime, WriteTime: result.WriteTime,
		SHA256: hex.EncodeToString(result.SHA256), SignatureCacheHit: result.SignatureCacheHit,
	}
	report := &acp.Report{Jobs: []*acp.Job{job}}
	if runErr != nil {
		report.Errors = []*acp.Error{{Src: item.Path, Err: runErr}}
	}

	// The row has one final destination, including errors before a target could start.
	err = errors.Join(runErr, result.Err)
	for _, target := range result.Targets {
		err = errors.Join(err, target.Err)
	}
	if err == nil && len(result.Targets) != 1 {
		err = fmt.Errorf("rewrite returned %d targets, want one", len(result.Targets))
	}
	if err != nil {
		return report, err
	}
	job.SuccessTargets = []string{item.Path}
	return report, nil
}

func relink(entry rewriteEntry, state *rewriteState, statePath string) error {
	for _, link := range entry.Links {
		if link == entry.Path {
			continue
		}
		if err := relinkOne(entry.Path, link, state, statePath); err != nil {
			return err
		}
	}
	return nil
}

func relinkOne(src, link string, state *rewriteState, statePath string) (err error) {
	// Link atomically allocates the temporary; existing names are never overwritten or removed.
	tmp, err := fileio.Link(src, filepath.Dir(link))
	if err != nil {
		return fmt.Errorf("create temporary hardlink failed, %w", err)
	}
	defer func() {
		if tmp != "" {
			err = errors.Join(err, cleanupTmpFile(state, tmp))
		}
	}()
	state.TmpFiles = append(state.TmpFiles, tmp)
	state.saved = false
	if err := writeState(statePath, state); err != nil {
		return &persistenceError{err}
	}

	// The new link already shares the rewritten inode's restored metadata.
	// A failed rename leaves the old link intact.
	if err := renameFile(tmp, link); err != nil {
		return fmt.Errorf("replace hardlink failed, %w", err)
	}
	state.TmpFiles = removeTmp(state.TmpFiles, tmp)
	state.saved = false
	tmp = ""
	return nil
}
