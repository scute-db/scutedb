package disktree

import (
	"bytes"
	"fmt"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/index"
)

type Iterator struct {
	t         *Tree
	from, to  []byte
	last      []byte
	keys      [][]byte
	rows      []core.RowID
	pos       int
	key       []byte
	rid       core.RowID
	err       error
	exhausted bool
	done      bool
}

func (t *Tree) Scan(from, to []byte) *Iterator {
	it := &Iterator{t: t}
	if from != nil {
		it.from = append([]byte{}, from...)
	}
	if to != nil {
		it.to = append([]byte{}, to...)
	}
	return it
}

func (it *Iterator) Next() bool {
	if it.done {
		return false
	}
	for it.pos >= len(it.keys) {
		if it.exhausted {
			it.done = true
			return false
		}
		if err := it.refill(); err != nil {
			it.err = err
			it.done = true
			return false
		}
	}
	it.key, it.rid = it.keys[it.pos], it.rows[it.pos]
	it.pos++
	it.last = it.key
	return true
}

func (it *Iterator) refill() error {
	it.keys, it.rows, it.pos = it.keys[:0], it.rows[:0], 0
	seek, after := it.from, false
	if it.last != nil {
		seek, after = it.last, true
	}
	f, n, err := it.t.descend(seek, nil)
	if err != nil {
		return err
	}
	seen := map[core.PageID]bool{f.ID(): true}
	for {
		i, found := n.Search(seek)
		if found && after {
			i++
		}
		for ; i < n.KeyCount(); i++ {
			k := n.KeyRef(i)
			if it.to != nil && bytes.Compare(k, it.to) >= 0 {
				it.exhausted = true
				break
			}
			it.keys = append(it.keys, append([]byte(nil), k...))
			it.rows = append(it.rows, n.Row(i))
		}
		next := n.Next()
		if err := it.t.unpin(f, false, nil); err != nil {
			return err
		}
		if next == 0 {
			it.exhausted = true
		}
		if len(it.keys) > 0 || it.exhausted {
			return nil
		}
		if seen[next] {
			return fmt.Errorf("%w: the leaf chain loops back to page %d", ErrCorrupt, next)
		}
		seen[next] = true
		if f, n, err = it.t.fetch(next); err != nil {
			return err
		}
		if !n.Leaf() {
			return it.t.unpin(f, false, fmt.Errorf("%w: leaf chain reaches page %d, which is not a leaf", ErrCorrupt, next))
		}
	}
}

func (it *Iterator) Key() []byte       { return it.key }
func (it *Iterator) RowID() core.RowID { return it.rid }
func (it *Iterator) Err() error        { return it.err }

func (it *Iterator) Close() error {
	it.done = true
	it.keys, it.rows = nil, nil
	return nil
}

var _ index.Iterator = (*Iterator)(nil)
