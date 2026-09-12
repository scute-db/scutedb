package btree

import (
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/index"
)

type Index struct{ t *Tree }

func NewIndex(order int) (*Index, error) {
	t, err := New(order)
	if err != nil {
		return nil, err
	}
	return &Index{t: t}, nil
}

func (x *Index) Tree() *Tree { return x.t }

func (x *Index) Get(key []byte) (core.RowID, error) {
	rid, ok := x.t.Get(key)
	if !ok {
		return core.RowID{}, core.ErrNotFound
	}
	return rid, nil
}

func (x *Index) Put(key []byte, rid core.RowID) error {
	x.t.Put(key, rid)
	return nil
}

func (x *Index) Delete(key []byte) error {
	if !x.t.Delete(key) {
		return core.ErrNotFound
	}
	return nil
}

func (x *Index) Scan(from, to []byte) (index.Iterator, error) {
	return x.t.Scan(from, to), nil
}

func (x *Index) Close() error { return nil }

var _ index.Index = (*Index)(nil)
