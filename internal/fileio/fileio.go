package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)

	Sync() error
	Size() (int64, error)
	Close() error
}

type OSFile struct{ f *os.File }

func Open(path string) (*OSFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return &OSFile{f: f}, nil
}

func OpenExisting(path string) (*OSFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &OSFile{f: f}, nil
}

func (o *OSFile) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o *OSFile) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }
func (o *OSFile) Sync() error                              { return o.f.Sync() }
func (o *OSFile) Close() error                             { return o.f.Close() }

func (o *OSFile) Size() (int64, error) {
	st, err := o.f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

var syncFile = func(f *os.File) error { return f.Sync() }

func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = syncFile(d)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
		return nil
	}
	return err
}

const maxLinks = 40

func SyncParent(path string) error {
	for hops := 0; ; hops++ {
		if hops > maxLinks {
			return &os.PathError{Op: "syncparent", Path: path, Err: syscall.ELOOP}
		}
		dir := rawDir(path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return SyncDir(dir)
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			path = target
		} else {
			path = dir + string(os.PathSeparator) + target
		}
	}
}

func rawDir(path string) string {
	i := strings.LastIndexByte(path, os.PathSeparator)
	switch {
	case i < 0:
		return "."
	case i == 0:
		return string(os.PathSeparator)
	}
	return path[:i]
}

var _ File = (*OSFile)(nil)
