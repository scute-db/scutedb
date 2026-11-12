package disktree

import (
	"bytes"
	"fmt"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
)

type Shape struct {
	Height      int
	Leaves      int
	Internal    int
	Keys        int
	EmptyLeaves int
	LeafBytes   int
}

func (s Shape) Pages() int { return s.Leaves + s.Internal }

func (s Shape) LeafFill() float64 {
	if s.Leaves == 0 {
		return 0
	}
	return float64(s.LeafBytes) / float64(s.Leaves*nodepage.EntryArea)
}

type leafLink struct {
	id, next core.PageID
}

type walker struct {
	t      *Tree
	seen   map[core.PageID]bool
	leaves []leafLink
	shape  Shape
}

func (t *Tree) Validate() error {
	_, err := t.Shape()
	return err
}

func (t *Tree) Shape() (Shape, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.measure()
}

func (t *Tree) measure() (Shape, error) {
	w := &walker{t: t, seen: map[core.PageID]bool{}}
	level, err := w.walk(t.root, -1, nil, nil)
	if err != nil {
		return Shape{}, err
	}
	w.shape.Height = level + 1
	for i, l := range w.leaves {
		want := core.PageID(0)
		if i+1 < len(w.leaves) {
			want = w.leaves[i+1].id
		}
		if l.next != want {
			return Shape{}, fmt.Errorf("%w: leaf %d links to page %d, the next leaf in key order is %d",
				ErrCorrupt, l.id, l.next, want)
		}
	}
	return w.shape, nil
}

func (w *walker) walk(id core.PageID, want int, low, high []byte) (int, error) {
	if w.seen[id] {
		return 0, fmt.Errorf("%w: page %d is reached twice", ErrCorrupt, id)
	}
	w.seen[id] = true
	f, err := w.t.pool.Fetch(id)
	if err != nil {
		return 0, err
	}
	w.t.pool.RLatch(f)
	v := view{f: f, own: true}
	n, err := nodepage.Load(f.Page())
	if err == nil {
		err = n.Validate()
	}
	if err != nil {
		return 0, w.t.release(v, fmt.Errorf("%w: page %d: %w", ErrCorrupt, id, err))
	}
	f.MarkVerified()
	level := int(n.Level())
	img := readImage(n)
	used := n.UsedBytes()
	if err := w.t.release(v, nil); err != nil {
		return 0, err
	}
	if want >= 0 && level != want {
		return 0, fmt.Errorf("%w: page %d is at level %d, its parent expects %d", ErrCorrupt, id, level, want)
	}

	for _, e := range img.entries {
		below := low != nil && bytes.Compare(e.key, low) < 0
		if !img.leaf && low != nil && bytes.Equal(e.key, low) {
			below = true
		}
		if below || (high != nil && bytes.Compare(e.key, high) >= 0) {
			return 0, fmt.Errorf("%w: page %d holds key % 02X outside its range [% 02X, % 02X)",
				ErrCorrupt, id, e.key, low, high)
		}
	}

	if img.leaf {
		w.shape.Leaves++
		w.shape.Keys += len(img.entries)
		w.shape.LeafBytes += used
		if len(img.entries) == 0 {
			w.shape.EmptyLeaves++
		}
		w.leaves = append(w.leaves, leafLink{id: id, next: img.link})
		return level, nil
	}

	w.shape.Internal++
	for i := 0; i <= len(img.entries); i++ {
		child, lo, hi := img.link, low, high
		if i > 0 {
			child, lo = img.entries[i-1].child, img.entries[i-1].key
		}
		if i < len(img.entries) {
			hi = img.entries[i].key
		}
		if _, err := w.walk(child, level-1, lo, hi); err != nil {
			return 0, err
		}
	}
	return level, nil
}
