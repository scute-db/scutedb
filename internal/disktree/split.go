package disktree

import (
	"errors"
	"fmt"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

type entry struct {
	key   []byte
	rid   core.RowID
	child core.PageID
}

type image struct {
	id      core.PageID
	leaf    bool
	level   uint16
	link    core.PageID
	entries []entry
}

func readImage(n nodepage.Node) image {
	img := image{id: n.ID(), leaf: n.Leaf(), level: n.Level(), entries: make([]entry, n.KeyCount())}
	if img.leaf {
		img.link = n.Next()
	} else {
		img.link = n.FirstChild()
	}
	for i := range img.entries {
		e := entry{key: n.Key(i)}
		if img.leaf {
			e.rid = n.Row(i)
		} else {
			e.child = n.Child(i + 1)
		}
		img.entries[i] = e
	}
	return img
}

func (img *image) insert(i int, e entry) {
	img.entries = append(img.entries, entry{})
	copy(img.entries[i+1:], img.entries[i:])
	img.entries[i] = e
}

func (img image) suffix() int {
	if img.leaf {
		return nodepage.LeafSuffix
	}
	return nodepage.InternalSuffix
}

func (img image) size(e entry) int { return nodepage.SlotSize + len(e.key) + img.suffix() }

func (img image) bytes() int {
	total := 0
	for _, e := range img.entries {
		total += img.size(e)
	}
	return total
}

func (img image) render() (page.Page, error) {
	var n nodepage.Node
	if img.leaf {
		n = nodepage.NewLeaf(img.id)
		n.SetNext(img.link)
	} else {
		var err error
		if n, err = nodepage.NewInternal(img.id, img.level); err != nil {
			return nil, err
		}
		n.SetFirstChild(img.link)
	}
	for i, e := range img.entries {
		var err error
		if img.leaf {
			err = n.InsertLeaf(i, e.key, e.rid)
		} else {
			err = n.InsertInternal(i, e.key, e.child)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: rebuilding page %d: %w", ErrCorrupt, img.id, err)
		}
	}
	return n.Page, nil
}

func chooseSplit(img image) int {
	n := len(img.entries)
	prefix := make([]int, n+1)
	for i, e := range img.entries {
		prefix[i+1] = prefix[i] + img.size(e)
	}
	total := prefix[n]
	best, bestCost := -1, 0
	for m := 1; m < n; m++ {
		left, right := prefix[m], total-prefix[m]
		if !img.leaf {
			if m+1 >= n {
				break
			}
			right = total - prefix[m+1]
		}
		if left > nodepage.EntryArea || right > nodepage.EntryArea {
			continue
		}
		cost := left
		if right > cost {
			cost = right
		}
		if best < 0 || cost < bestCost {
			best, bestCost = m, cost
		}
	}
	return best
}

func (img image) halves(m int, rightID core.PageID) (image, image, []byte) {
	left := image{id: img.id, leaf: img.leaf, level: img.level, link: img.link, entries: img.entries[:m]}
	right := image{id: rightID, leaf: img.leaf, level: img.level}
	sep := img.entries[m].key
	if img.leaf {
		right.link = img.link
		left.link = rightID
		right.entries = img.entries[m:]
	} else {
		right.link = img.entries[m].child
		right.entries = img.entries[m+1:]
	}
	return left, right, sep
}

type write struct {
	id    core.PageID
	bytes page.Page
	fresh bool
}

func (t *Tree) split(path []step, cur image) error {
	var writes []write
	var fresh []core.PageID
	fail := func(err error) error {
		var errs []error
		for _, id := range fresh {
			if ferr := t.pool.Free(id); ferr != nil {
				errs = append(errs, ferr)
			}
		}
		return errors.Join(append([]error{err}, errs...)...)
	}
	emit := func(img image, isNew bool) error {
		b, err := img.render()
		if err != nil {
			return err
		}
		writes = append(writes, write{id: img.id, bytes: b, fresh: isNew})
		return nil
	}
	alloc := func(kind page.Kind) (core.PageID, error) {
		f, err := t.pool.NewPage(kind)
		if err != nil {
			return 0, err
		}
		id := f.ID()
		fresh = append(fresh, id)
		return id, t.unpin(f, false, nil)
	}

	for {
		if cur.bytes() <= nodepage.EntryArea {
			if err := emit(cur, false); err != nil {
				return fail(err)
			}
			break
		}
		m := chooseSplit(cur)
		if m < 0 {
			return fail(fmt.Errorf("%w: page %d with %d entries cannot be split", ErrCorrupt, cur.id, len(cur.entries)))
		}
		kind := page.KindBTreeInternal
		if cur.leaf {
			kind = page.KindBTreeLeaf
		}

		if cur.id == t.root {
			a, err := alloc(kind)
			if err != nil {
				return fail(err)
			}
			b, err := alloc(kind)
			if err != nil {
				return fail(err)
			}
			left, right, sep := cur.halves(m, b)
			left.id = a
			root := image{id: t.root, level: cur.level + 1, link: a, entries: []entry{{key: sep, child: b}}}
			for _, w := range []struct {
				img   image
				isNew bool
			}{{left, true}, {right, true}, {root, false}} {
				if err := emit(w.img, w.isNew); err != nil {
					return fail(err)
				}
			}
			break
		}

		b, err := alloc(kind)
		if err != nil {
			return fail(err)
		}
		left, right, sep := cur.halves(m, b)
		if err := emit(right, true); err != nil {
			return fail(err)
		}
		if err := emit(left, false); err != nil {
			return fail(err)
		}

		up := path[len(path)-1]
		path = path[:len(path)-1]
		pf, pn, err := t.fetch(up.id)
		if err != nil {
			return fail(err)
		}
		moved := int(pn.Level()) != int(cur.level)+1 || up.index > pn.KeyCount() || pn.Child(up.index) != cur.id
		parent := readImage(pn)
		if err := t.unpin(pf, false, nil); err != nil {
			return fail(err)
		}
		if moved {
			return fail(fmt.Errorf("%w: page %d no longer points at page %d", ErrCorrupt, up.id, cur.id))
		}
		parent.insert(up.index, entry{key: sep, child: b})
		cur = parent
	}
	t.pending = order(writes)
	err := t.resume()
	if untouched(t.pending, writes) {
		t.pending = nil
		return fail(err)
	}
	return err
}

func order(writes []write) []write {
	var out []write
	for _, w := range writes {
		if w.fresh {
			out = append(out, w)
		}
	}
	for i := len(writes) - 1; i >= 0; i-- {
		if !writes[i].fresh {
			out = append(out, writes[i])
		}
	}
	return out
}

func untouched(pending, writes []write) bool {
	old := 0
	for _, w := range writes {
		if !w.fresh {
			old++
		}
	}
	return len(pending) > 0 && len(pending) >= old
}

func (t *Tree) resume() error {
	var errs []error
	for len(t.pending) > 0 {
		w := t.pending[0]
		f, err := t.pool.Fetch(w.id)
		if err != nil {
			errs = append(errs, fmt.Errorf("scutedb/disktree: finishing a split at page %d: %w", w.id, err))
			break
		}
		copy(f.Page(), w.bytes)
		f.MarkVerified()
		t.pending = t.pending[1:]
		if err := t.unpin(f, true, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
