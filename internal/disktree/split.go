package disktree

import (
	"bytes"
	"fmt"

	"github.com/scute-db/scutedb/internal/bufpool"
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

type level struct {
	img   image
	snap  page.Page
	at    int
	slot  int
	frame *bufpool.Frame
	left  *bufpool.Frame
	right *bufpool.Frame
}

func (t *Tree) split(m *bufpool.MTR, path []step, cur image) error {
	t.at("split")
	levels, err := t.plan(m, path, cur)
	if err != nil {
		return err
	}
	t.at("latch")
	if err := t.claim(m, levels); err != nil {
		return err
	}
	return t.apply(levels)
}

func (t *Tree) snapshot(m *bufpool.MTR, id core.PageID) (view, page.Page, error) {
	v, err := t.read(m, id)
	if err != nil {
		return view{}, nil, err
	}
	return v, append(page.Page(nil), v.f.Page()...), nil
}

func (t *Tree) plan(m *bufpool.MTR, path []step, cur image) ([]level, error) {
	v, snap, err := t.snapshot(m, cur.id)
	if err != nil {
		return nil, err
	}
	if err := t.release(v, nil); err != nil {
		return nil, err
	}
	var levels []level
	slot := -1
	for {
		lv := level{img: cur, snap: snap, at: -1, slot: slot}
		if cur.bytes() <= nodepage.EntryArea {
			return append(levels, lv), nil
		}
		if lv.at = chooseSplit(cur); lv.at < 0 {
			return nil, fmt.Errorf("%w: page %d with %d entries cannot be split", ErrCorrupt, cur.id, len(cur.entries))
		}
		levels = append(levels, lv)
		if cur.id == t.root {
			return levels, nil
		}
		up := path[len(path)-1]
		path = path[:len(path)-1]
		pv, psnap, err := t.snapshot(m, up.id)
		if err != nil {
			return nil, err
		}
		moved := int(pv.n.Level()) != int(cur.level)+1 || up.index > pv.n.KeyCount() || pv.n.Child(up.index) != cur.id
		parent := readImage(pv.n)
		if err := t.release(pv, nil); err != nil {
			return nil, err
		}
		if moved {
			return nil, fmt.Errorf("%w: page %d no longer points at page %d", ErrCorrupt, up.id, cur.id)
		}
		parent.insert(up.index, entry{key: cur.entries[lv.at].key})
		slot, snap, cur = up.index, psnap, parent
	}
}

func (t *Tree) claim(m *bufpool.MTR, levels []level) error {
	for k := len(levels) - 1; k >= 0; k-- {
		lv := &levels[k]
		f, err := m.Fetch(lv.img.id)
		if err != nil {
			return err
		}
		if err := m.WLatch(f); err != nil {
			return err
		}
		if !bytes.Equal(f.Page(), lv.snap) {
			return fmt.Errorf("%w: page %d changed between planning a split and latching it", ErrCorrupt, lv.img.id)
		}
		lv.frame = f
	}
	for k := range levels {
		lv := &levels[k]
		if lv.at < 0 {
			continue
		}
		kind := page.KindBTreeInternal
		if lv.img.leaf {
			kind = page.KindBTreeLeaf
		}
		var err error
		if lv.img.id == t.root {
			if lv.left, err = fresh(m, kind); err != nil {
				return err
			}
		}
		if lv.right, err = fresh(m, kind); err != nil {
			return err
		}
	}
	return nil
}

func fresh(m *bufpool.MTR, kind page.Kind) (*bufpool.Frame, error) {
	f, err := m.NewPage(kind)
	if err != nil {
		return nil, err
	}
	return f, m.WLatch(f)
}

func (t *Tree) apply(levels []level) error {
	var below core.PageID
	for k := range levels {
		lv := &levels[k]
		if lv.slot >= 0 {
			lv.img.entries[lv.slot].child = below
		}
		if lv.at < 0 {
			return draw(lv.frame, lv.img)
		}
		left, right, sep := lv.img.halves(lv.at, lv.right.ID())
		if lv.img.id == t.root {
			left.id = lv.left.ID()
			root := image{id: t.root, level: lv.img.level + 1, link: left.id, entries: []entry{{key: sep, child: right.id}}}
			if err := draw(lv.left, left); err != nil {
				return err
			}
			if err := draw(lv.right, right); err != nil {
				return err
			}
			return draw(lv.frame, root)
		}
		if err := draw(lv.right, right); err != nil {
			return err
		}
		if err := draw(lv.frame, left); err != nil {
			return err
		}
		below = right.id
	}
	return nil
}

func draw(f *bufpool.Frame, img image) error {
	p, err := img.render()
	if err != nil {
		return err
	}
	copy(f.Page(), p)
	f.MarkVerified()
	return nil
}
