package disktree

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func wideKey(i int) []byte {
	k := bytes.Repeat([]byte{'k'}, 100)
	copy(k[92:], k8(i))
	return k
}

type onDisk struct {
	t    *testing.T
	src  *bufpool.MemSource
	root core.PageID
}

func buildOnDisk(t *testing.T, n int, deleteAll bool) *onDisk {
	t.Helper()
	src := bufpool.NewMemSource()
	tr, root, err := Create(newPool(t, src, 8, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		mustPut(t, tr, wideKey(i), ridOf(i))
	}
	if deleteAll {
		for i := 0; i < n; i++ {
			if _, err := tr.Delete(wideKey(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if s := mustValidate(t, tr); s.Height != 3 {
		t.Fatalf("test tree has height %d, want 3", s.Height)
	}
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	return &onDisk{t: t, src: src, root: root}
}

func (d *onDisk) node(id core.PageID) nodepage.Node {
	d.t.Helper()
	p, err := d.src.Read(id)
	if err != nil {
		d.t.Fatal(err)
	}
	n, err := nodepage.Load(p)
	if err != nil {
		d.t.Fatal(err)
	}
	return n
}

func (d *onDisk) put(n nodepage.Node) {
	d.t.Helper()
	if err := d.src.Write(n.Page); err != nil {
		d.t.Fatal(err)
	}
}

func (d *onDisk) leaves() []core.PageID {
	id := d.root
	n := d.node(id)
	for !n.Leaf() {
		id = n.Child(0)
		n = d.node(id)
	}
	var out []core.PageID
	for id != 0 {
		out = append(out, id)
		id = d.node(id).Next()
	}
	return out
}

func (d *onDisk) open() (*Tree, error) {
	return Open(newPool(d.t, d.src, 5, bufpool.LRU, bufpool.WriteBack), d.root)
}

func poke(t *testing.T, tr *Tree) {
	t.Helper()
	for i := 0; i < 40; i++ {
		_, _, _ = tr.Get(wideKey(i * 37))
		noPins(t, tr, "Get on a corrupt tree")
		_ = tr.Put(wideKey(i*41+5), ridOf(i))
		noPins(t, tr, "Put on a corrupt tree")
		_, _ = tr.Delete(wideKey(i * 43))
		noPins(t, tr, "Delete on a corrupt tree")
	}
	drain(t, tr.Scan(nil, nil), 1_000_000)
	noPins(t, tr, "Scan on a corrupt tree")
	_, _ = tr.Height()
	_, _ = tr.Shape()
	noPins(t, tr, "Shape on a corrupt tree")
}

func TestValidateCatchesCorruptTrees(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(d *onDisk)
		want    error
	}{
		{"a leaf link skips its neighbour", func(d *onDisk) {
			l := d.leaves()
			n := d.node(l[3])
			n.SetNext(l[5])
			d.put(n)
		}, ErrCorrupt},
		{"the last leaf links back to the first", func(d *onDisk) {
			l := d.leaves()
			n := d.node(l[len(l)-1])
			n.SetNext(l[0])
			d.put(n)
		}, ErrCorrupt},
		{"a separator moved so keys fall outside their node", func(d *onDisk) {
			root := d.node(d.root)
			img := readImage(root)
			a, b := img.entries[0].key, img.entries[1].key
			mid := append([]byte(nil), a...)
			for i := range mid {
				if a[i] != b[i] {
					mid[i] = a[i] + (b[i]-a[i])/2
					break
				}
			}
			if bytes.Equal(mid, a) {
				mid = append(mid, 0xFF)
			}
			img.entries[0].key = mid
			p, err := img.render()
			if err != nil {
				t.Fatal(err)
			}
			d.put(nodepage.Node{Page: p})
		}, ErrCorrupt},
		{"an internal key equals the bound its parent gave it", func(d *onDisk) {
			root := d.node(d.root)
			bound := root.Key(0)
			mid := readImage(d.node(root.Child(1)))
			mid.entries[0].key = bound
			p, err := mid.render()
			if err != nil {
				t.Fatal(err)
			}
			d.put(nodepage.Node{Page: p})
		}, ErrCorrupt},
		{"a child sits at the wrong level", func(d *onDisk) {
			root := d.node(d.root)
			if err := root.SetChild(0, d.leaves()[0]); err != nil {
				t.Fatal(err)
			}
			d.put(root)
		}, ErrCorrupt},
		{"a child points back at the root", func(d *onDisk) {
			mid := d.node(d.node(d.root).Child(1))
			if err := mid.SetChild(0, d.root); err != nil {
				t.Fatal(err)
			}
			d.put(mid)
		}, ErrCorrupt},
		{"two children point at the same page", func(d *onDisk) {
			root := d.node(d.root)
			if err := root.SetChild(1, root.Child(0)); err != nil {
				t.Fatal(err)
			}
			d.put(root)
		}, ErrCorrupt},
		{"a leaf is full of garbage", func(d *onDisk) {
			l := d.leaves()
			n := d.node(l[2])
			rand.New(rand.NewSource(1)).Read(n.Page[page.HeaderSize:])
			d.put(n)
		}, ErrCorrupt},
		{"a leaf now says it is a heap page", func(d *onDisk) {
			n := d.node(d.leaves()[1])
			n.SetKind(page.KindHeap)
			d.put(n)
		}, nodepage.ErrBadKind},
		{"a child points at a page that does not exist", func(d *onDisk) {
			mid := d.node(d.node(d.root).Child(0))
			if err := mid.SetChild(1, 9999); err != nil {
				t.Fatal(err)
			}
			d.put(mid)
		}, bufpool.ErrNoSuchPage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := buildOnDisk(t, 2000, false)
			c.corrupt(d)
			tr, err := d.open()
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := tr.Validate(); !errors.Is(err, c.want) {
				t.Fatalf("Validate gave %v, want %v", err, c.want)
			}
			noPins(t, tr, "Validate")
			poke(t, tr)
		})
	}
}

func TestAScanOverALoopingLeafChainStopsWithAnError(t *testing.T) {
	for _, empty := range []bool{false, true} {
		d := buildOnDisk(t, 2000, empty)
		l := d.leaves()
		n := d.node(l[len(l)-1])
		n.SetNext(l[0])
		d.put(n)
		tr, err := d.open()
		if err != nil {
			t.Fatal(err)
		}
		it := tr.Scan(nil, nil)
		count := drain(t, it, 1_000_000)
		if !errors.Is(it.Err(), ErrCorrupt) {
			t.Fatalf("empty %v: scan over a looping chain ended with %v after %d keys", empty, it.Err(), count)
		}
		if !empty && count != 2000 {
			t.Fatalf("scan returned %d keys before spotting the loop, want all 2000", count)
		}
		noPins(t, tr, "a looping scan")
	}
}

func TestAChainThatReachesAnInternalPageIsRejected(t *testing.T) {
	d := buildOnDisk(t, 2000, true)
	l := d.leaves()
	n := d.node(l[0])
	n.SetNext(d.root)
	d.put(n)
	tr, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	it := tr.Scan(nil, nil)
	if it.Next() || !errors.Is(it.Err(), ErrCorrupt) {
		t.Fatalf("scan gave %v", it.Err())
	}
	noPins(t, tr, "the scan")
}

func TestOpenRefusesACorruptRoot(t *testing.T) {
	d := buildOnDisk(t, 2000, false)
	root := d.node(d.root)
	root.SetFreeStart(root.FreeStart() + 4)
	d.put(root)
	if _, err := d.open(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open on a root with a broken header gave %v", err)
	}
}
