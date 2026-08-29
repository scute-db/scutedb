package fileio

import "os"

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

var _ File = (*OSFile)(nil)
