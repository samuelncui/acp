package acp

import (
	"context"
	"errors"
	"io/fs"
)

// SourceFromFS binds a filesystem and its relative, slash-delimited name without opening it.
// ACP opens the file in the ordered source lane, observes Stat and optional Xattrs, and owns Close.
// Invalid names and a nil filesystem fail that item before target creation. The filesystem must
// remain disjoint from native outputs; ACP cannot identify aliases hidden behind an fs.FS.
func SourceFromFS(fsys fs.FS, name string) ContentSource {
	return fsSource{fsys: fsys, name: name}
}

type fsSource struct {
	fsys fs.FS
	name string
}

func (s fsSource) Open(ctx context.Context) (fs.File, error) {
	// Refuse invalid bindings and cancellation without consuming a filesystem member.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.fsys == nil || !fs.ValidPath(s.name) {
		return nil, &fs.PathError{Op: "open", Path: s.name, Err: fs.ErrInvalid}
	}

	// Keep any acquired handle for ACP's sole Close, including on error or cancellation.
	file, err := s.fsys.Open(s.name)
	return file, errors.Join(err, ctx.Err())
}
