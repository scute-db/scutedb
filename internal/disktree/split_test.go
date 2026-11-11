package disktree

import (
	"bytes"
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
)

func entries(sizes ...int) []entry {
	out := make([]entry, len(sizes))
	for i, n := range sizes {
		k := bytes.Repeat([]byte{byte('a' + i)}, n)
		out[i] = entry{key: k, rid: core.RowID{Page: core.PageID(i + 1)}, child: core.PageID(i + 10)}
	}
	return out
}

func TestChooseSplitBalancesBytesNotCounts(t *testing.T) {
	leaf := image{leaf: true, entries: entries(1000, 1000, 10, 10, 10, 10, 10, 10, 1000, 1000)}
	m := chooseSplit(leaf)
	left := image{leaf: true, entries: leaf.entries[:m]}.bytes()
	right := image{leaf: true, entries: leaf.entries[m:]}.bytes()
	if m != 5 {
		t.Fatalf("split at %d (left %d bytes, right %d); want 5, the byte midpoint", m, left, right)
	}

	internal := image{level: 1, entries: entries(1000, 1000, 1000, 1000, 1000)}
	if m := chooseSplit(internal); m != 2 {
		t.Fatalf("internal split at %d, want 2: two keys left, one up, two right", m)
	}
	l, r, sep := internal.halves(2, 77)
	if len(l.entries) != 2 || len(r.entries) != 2 || sep[0] != 'c' || r.link != internal.entries[2].child || r.id != 77 {
		t.Fatalf("halves: left %d, right %d, sep %q, right first child %d", len(l.entries), len(r.entries), sep[:1], r.link)
	}
	if l.bytes() > nodepage.EntryArea || r.bytes() > nodepage.EntryArea {
		t.Fatal("a half does not fit in a page")
	}

	if m := chooseSplit(image{level: 1, entries: entries(5, 5)}); m != -1 {
		t.Fatalf("an internal node with two keys has no middle key to push up, but chooseSplit said %d", m)
	}
	if m := chooseSplit(image{leaf: true, entries: entries(5)}); m != -1 {
		t.Fatalf("a single entry cannot be split, but chooseSplit said %d", m)
	}
}

func TestRenderRefusesEntriesThatWouldBreakTheNode(t *testing.T) {
	bad := image{id: 3, leaf: true, entries: entries(4, 4)}
	bad.entries[0], bad.entries[1] = bad.entries[1], bad.entries[0]
	if _, err := bad.render(); !errors.Is(err, ErrCorrupt) || !errors.Is(err, nodepage.ErrOrder) {
		t.Fatalf("render of out-of-order entries gave %v", err)
	}
	if _, err := (image{id: 3, level: 0, entries: entries(4)}).render(); !errors.Is(err, nodepage.ErrBadLevel) {
		t.Fatalf("render of an internal node at level 0 gave %v", err)
	}
	p, err := image{id: 3, level: 2, link: 9, entries: entries(4, 4)}.render()
	if err != nil {
		t.Fatal(err)
	}
	n, err := nodepage.Load(p)
	if err != nil || n.Validate() != nil || n.FirstChild() != 9 || n.Child(2) != 11 || n.Level() != 2 {
		t.Fatalf("rendered internal node is wrong: %v", err)
	}
}
