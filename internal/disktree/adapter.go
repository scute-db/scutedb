package disktree

import (
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/index"
)

type Index struct{ t *Tree }

func NewIndex(t *Tree) *Index { return &Index{t: t} }

func (x *Index) Tree() *Tree { return x.t }

func (x *Index) Get(key []byte) (core.RowID, error) {
	rid, ok, err := x.t.Get(key)
	if err != nil {
		return core.RowID{}, err
	}
	if !ok {
		return core.RowID{}, core.ErrNotFound
	}
	return rid, nil
}

func (x *Index) Put(key []byte, rid core.RowID) error { return x.t.Put(key, rid) }

func (x *Index) Delete(key []byte) error {
	ok, err := x.t.Delete(key)
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrNotFound
	}
	return nil
}

func (x *Index) Scan(from, to []byte) (index.Iterator, error) {
	return x.t.Scan(from, to), nil
}

func (x *Index) Close() error {
	if err := x.t.resume(); err != nil {
		return err
	}
	return x.t.pool.FlushAll()
}

var _ index.Index = (*Index)(nil)
