package disktree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func TestGetOfAMissingKeyReturnsTheZeroRowID(t *testing.T) {
	tr, _ := newTree(t, 4)
	mustPut(t, tr, []byte("b"), ridOf(707))
	rid, ok, err := tr.Get([]byte("a"))
	if ok || err != nil || rid != (core.RowID{}) {
		t.Fatalf("Get of a missing key = %+v %v %v, want the zero RowID", rid, ok, err)
	}
}

func TestValidateCatchesAKeyAtOrAboveItsHighBound(t *testing.T) {
	d := buildOnDisk(t, 2000, false)
	root := d.node(d.root)
	n := d.node(root.Child(0))
	for !n.Leaf() {
		n = d.node(n.Child(n.ChildCount() - 1))
	}
	img := readImage(root)
	img.entries[0].key = n.Key(n.KeyCount() - 1)
	p, err := img.render()
	if err != nil {
		t.Fatal(err)
	}
	d.put(nodepage.Node{Page: p})
	tr, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Validate(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Validate of a subtree holding its own high bound gave %v", err)
	}
	noPins(t, tr, "Validate")
}

func TestARootThatSkipsALevelIsCorrupt(t *testing.T) {
	d := buildOnDisk(t, 2000, false)
	img := readImage(d.node(d.root))
	img.level++
	p, err := img.render()
	if err != nil {
		t.Fatal(err)
	}
	d.put(nodepage.Node{Page: p})
	tr, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Validate(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Validate of a root one level too high gave %v", err)
	}
	if _, _, err := tr.Get(wideKey(5)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get through a root one level too high gave %v", err)
	}
	noPins(t, tr, "the checks")
}

func TestShapeCountsEveryPageTheTreeOwns(t *testing.T) {
	tr, src := newTree(t, 8)
	for i := 0; i < 3000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	s := mustValidate(t, tr)
	if s.Internal != 1 || s.Pages() != src.Len() || s.Pages() != s.Leaves+s.Internal {
		t.Fatalf("shape %+v says %d pages, the source holds %d", s, s.Pages(), src.Len())
	}
	if (Shape{}).LeafFill() != 0 {
		t.Fatal("a shape with no leaves must have zero fill")
	}
}

func fetches(tr *Tree) uint64 {
	s := tr.Pool().Stats()
	return s.Hits + s.Misses
}

func TestAScanReadsOneLeafPerRefillAndStopsAtItsBound(t *testing.T) {
	tr, _ := newTree(t, 8)
	for i := 0; i < 3000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	h, err := tr.Height()
	if err != nil || h != 2 {
		t.Fatalf("height %d %v, want 2", h, err)
	}
	before := fetches(tr)
	it := tr.Scan(nil, nil)
	if !it.Next() {
		t.Fatal(it.Err())
	}
	if got := fetches(tr) - before; got != uint64(h) {
		t.Fatalf("the first refill fetched %d pages, want %d: one descent, one leaf", got, h)
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}

	before = fetches(tr)
	if got := collect(t, tr.Scan(nil, k8(5))); len(got) != 5 {
		t.Fatalf("bounded scan returned %d keys", len(got))
	}
	if got := fetches(tr) - before; got != uint64(h) {
		t.Fatalf("a scan bounded inside the first leaf fetched %d pages, want %d", got, h)
	}
}

func TestAScanCopiesItsBounds(t *testing.T) {
	tr, _ := newTree(t, 4)
	for i := 0; i < 100; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	from, to := k8(10), k8(20)
	it := tr.Scan(from, to)
	for i := range from {
		from[i], to[i] = 0xFF, 0
	}
	if got := collect(t, it); len(got) != 10 || !bytes.Equal(got[0].key, k8(10)) {
		t.Fatalf("changing the caller's bound slices changed the scan: %d keys", len(got))
	}
}

func TestAnOversizedKeyIsRefusedBeforeAnySplitWork(t *testing.T) {
	fs := &faultSource{inner: bufpool.NewMemSource()}
	tr, _, err := Create(newPool(t, fs, 4, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 226; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	if h, _ := tr.Height(); h != 1 {
		t.Fatalf("height %d, the root leaf should be nearly full", h)
	}
	allocs := fs.allocs
	err = tr.Put(bytes.Repeat([]byte{0xFF}, nodepage.MaxKeySize+1), ridOf(1))
	if !errors.Is(err, nodepage.ErrKeyTooLarge) || errors.Is(err, ErrCorrupt) || fs.allocs != allocs {
		t.Fatalf("Put of an oversized key into a full leaf gave %v after %d allocations", err, fs.allocs-allocs)
	}
}

func TestTheIndexAdapterCloseFlushesToTheSource(t *testing.T) {
	src := bufpool.NewMemSource()
	tr, root, err := Create(newPool(t, src, 4, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	x := NewIndex(tr)
	if err := x.Put([]byte("k"), ridOf(3)); err != nil {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(newPool(t, src, 4, bufpool.LRU, bufpool.WriteBack), root)
	if err != nil {
		t.Fatal(err)
	}
	if rid, ok, err := again.Get([]byte("k")); !ok || err != nil || rid != ridOf(3) {
		t.Fatalf("after Close the source does not hold the key: %+v %v %v", rid, ok, err)
	}
}

type armedSource struct {
	inner      bufpool.Source
	armed      bool
	live       bool
	failWrites bool
	failAt     int
	count      int
}

func (s *armedSource) tick() bool {
	if !s.live {
		return false
	}
	s.count++
	return s.count == s.failAt
}

func (s *armedSource) Read(id core.PageID) (page.Page, error) {
	if !s.failWrites && s.tick() {
		return nil, errInjected
	}
	return s.inner.Read(id)
}

func (s *armedSource) Write(p page.Page) error {
	if s.failWrites && s.tick() {
		return errInjected
	}
	return s.inner.Write(p)
}

func (s *armedSource) Allocate(kind page.Kind) (page.Page, error) {
	if s.armed {
		s.live = true
	}
	return s.inner.Allocate(kind)
}

func (s *armedSource) Free(id core.PageID) error { return s.inner.Free(id) }

func TestAFailureWhileASplitWritesItsPagesKeepsEveryEarlierKeyReadable(t *testing.T) {
	hit := 0
	for _, frames := range []int{MinFrames(1), MinFrames(1) + 1} {
		for _, preload := range []int{200, 400} {
			for _, writes := range []bool{false, true} {
				for at := 1; at <= 8; at++ {
					src := &armedSource{inner: bufpool.NewMemSource(), failWrites: writes, failAt: at}
					tr, _, err := Create(newPool(t, src, frames, bufpool.LRU, bufpool.WriteBack))
					if err != nil {
						t.Fatal(err)
					}
					model := map[string]core.RowID{}
					for i := 0; i < preload; i++ {
						mustPut(t, tr, k8(i*2), ridOf(i))
						model[string(k8(i*2))] = ridOf(i)
					}
					src.armed = true
					for i := 0; i < preload; i++ {
						if err := tr.Put(k8(i*2+1), ridOf(i)); err != nil {
							hit++
							break
						}
						model[string(k8(i*2+1))] = ridOf(i)
					}
					src.armed, src.live = false, false
					for k, v := range model {
						rid, ok, err := tr.Get([]byte(k))
						if err != nil || !ok || rid != v {
							t.Fatalf("frames %d, preload %d, writes %v, fail at %d: Get(% 02X) = %+v %v %v after a failed split, want %+v",
								frames, preload, writes, at, k, rid, ok, err, v)
						}
					}
					noPins(t, tr, "a failed split")
				}
			}
		}
	}
	if hit == 0 {
		t.Fatal("no failure was ever injected")
	}
}

func TestAWriteThroughFailureDuringPutIsReported(t *testing.T) {
	fs := &faultSource{inner: bufpool.NewMemSource()}
	tr, _, err := Create(newPool(t, fs, 4, bufpool.LRU, bufpool.WriteThrough))
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, tr, []byte("a"), ridOf(1))
	fs.failWrite = fs.writes + 1
	if err := tr.Put([]byte("b"), ridOf(2)); !errors.Is(err, errInjected) {
		t.Fatalf("Put whose write-through failed gave %v", err)
	}
	noPins(t, tr, "a failed write-through")
}

func TestUnpinKeepsBothTheOperationErrorAndTheUnpinError(t *testing.T) {
	tr, _ := newTree(t, 4)
	f, err := tr.Pool().Fetch(tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.unpin(f, false, nil); err != nil {
		t.Fatal(err)
	}
	err = tr.unpin(f, false, errInjected)
	if !errors.Is(err, errInjected) || !errors.Is(err, bufpool.ErrNotPinned) {
		t.Fatalf("a failed unpin after a failed operation gave %v, want both errors", err)
	}
}

type pageFailSource struct {
	inner bufpool.Source
	bad   map[core.PageID]bool
}

func (s *pageFailSource) Read(id core.PageID) (page.Page, error) { return s.inner.Read(id) }

func (s *pageFailSource) Write(p page.Page) error {
	if s.bad[p.ID()] {
		return errInjected
	}
	return s.inner.Write(p)
}

func (s *pageFailSource) Allocate(kind page.Kind) (page.Page, error) { return s.inner.Allocate(kind) }

func (s *pageFailSource) Free(id core.PageID) error { return s.inner.Free(id) }

func TestADescentReportsAParentWhoseWriteThroughKeepsFailing(t *testing.T) {
	src := &pageFailSource{inner: bufpool.NewMemSource(), bad: map[core.PageID]bool{}}
	tr, root, err := Create(newPool(t, src, 4, bufpool.LRU, bufpool.WriteThrough))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		mustPut(t, tr, k8(i*2), ridOf(i))
	}
	src.bad[root] = true
	failed := false
	for i := 0; i < 1000 && !failed; i++ {
		failed = tr.Put(k8(i*2+1), ridOf(i)) != nil
	}
	if !failed {
		t.Fatal("no Put ever rewrote the root")
	}
	if _, _, err := tr.Get(k8(0)); !errors.Is(err, errInjected) {
		t.Fatalf("Get through a root whose write keeps failing gave %v", err)
	}
	noPins(t, tr, "the failing descent")
}

func TestChooseSplitCountsTheSeparatorOutOfTheRightHalfAndKeepsTheSmallerHalfOnATie(t *testing.T) {
	if m := chooseSplit(image{level: 1, entries: entries(10, 10, 1000, 10, 1000)}); m != 2 {
		t.Fatalf("internal split at %d, want 2: the separator moves up, so it is not part of the right half", m)
	}
	if m := chooseSplit(image{leaf: true, entries: entries(10, 10, 10, 10, 10)}); m != 2 {
		t.Fatalf("five equal entries split at %d, want 2", m)
	}
}

func TestImageBytesMatchTheRenderedNode(t *testing.T) {
	for _, img := range []image{
		{id: 4, leaf: true, entries: entries(1, 30, 999)},
		{id: 4, level: 3, link: 8, entries: entries(1, 30, 999)},
	} {
		p, err := img.render()
		if err != nil {
			t.Fatal(err)
		}
		n, err := nodepage.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if n.UsedBytes() != img.bytes() {
			t.Fatalf("leaf %v: image says %d bytes, the node uses %d", img.leaf, img.bytes(), n.UsedBytes())
		}
	}
}

func TestANodeThatExactlyFillsItsPageIsNotSplit(t *testing.T) {
	tr, src := newTree(t, 4)
	img := image{id: tr.Root(), leaf: true, entries: entries(1000, 1000, 1000, 1000, 22)}
	if img.bytes() != nodepage.EntryArea {
		t.Fatalf("test image holds %d bytes, want exactly %d", img.bytes(), nodepage.EntryArea)
	}
	m := tr.Pool().Begin()
	if err := tr.split(m, nil, img); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(); err != nil {
		t.Fatal(err)
	}
	if h, err := tr.Height(); h != 1 || err != nil || src.Len() != 1 {
		t.Fatalf("height %d %v, %d pages: a node that fits exactly was split", h, err, src.Len())
	}
	if s := mustValidate(t, tr); s.Keys != 5 {
		t.Fatalf("%d keys after rewriting the root, want 5", s.Keys)
	}
}

var errFree = errors.New("free failed")

type allocFailSource struct {
	inner    bufpool.Source
	allocs   int
	failAt   int
	freeFail bool
}

func (s *allocFailSource) Read(id core.PageID) (page.Page, error) { return s.inner.Read(id) }

func (s *allocFailSource) Write(p page.Page) error { return s.inner.Write(p) }

func (s *allocFailSource) Allocate(kind page.Kind) (page.Page, error) {
	s.allocs++
	if s.allocs == s.failAt {
		return nil, errInjected
	}
	return s.inner.Allocate(kind)
}

func (s *allocFailSource) Free(id core.PageID) error {
	if s.freeFail {
		return errFree
	}
	return s.inner.Free(id)
}

func TestARootSplitWhoseSecondAllocationFailsFreesTheFirstAndReportsAFailedFree(t *testing.T) {
	for _, freeFail := range []bool{false, true} {
		mem := bufpool.NewMemSource()
		src := &allocFailSource{inner: mem, freeFail: freeFail}
		tr, _, err := Create(newPool(t, src, 4, bufpool.LRU, bufpool.WriteBack))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 226; i++ {
			mustPut(t, tr, k8(i), ridOf(i))
		}
		src.failAt = src.allocs + 2
		pages := mem.Len()
		err = tr.Put(k8(1000), ridOf(1))
		if !errors.Is(err, errInjected) {
			t.Fatalf("free fails %v: Put gave %v", freeFail, err)
		}
		if freeFail && !errors.Is(err, errFree) {
			t.Fatalf("the failed Free of the first new page was not reported: %v", err)
		}
		if !freeFail && mem.Len() != pages {
			t.Fatalf("the failed root split leaked %d pages", mem.Len()-pages)
		}
		noPins(t, tr, "a failed root split")
	}
}

func TestASeparatorTooBigToSplitAroundIsCorruptNotAPanic(t *testing.T) {
	mem := bufpool.NewMemSource()
	alloc := func(kind page.Kind) core.PageID {
		p, err := mem.Allocate(kind)
		if err != nil {
			t.Fatal(err)
		}
		return p.ID()
	}
	rootID, left, right := alloc(page.KindBTreeInternal), alloc(page.KindBTreeLeaf), alloc(page.KindBTreeLeaf)
	l := nodepage.NewLeaf(left)
	l.SetNext(right)
	r := nodepage.NewLeaf(right)
	root, err := nodepage.NewInternal(rootID, 1)
	if err != nil {
		t.Fatal(err)
	}
	root.SetFirstChild(left)
	key := bytes.Repeat([]byte{'m'}, 4060)
	start := page.Size - len(key) - nodepage.InternalSuffix
	copy(root.Page[start:], key)
	binary.BigEndian.PutUint32(root.Page[start+len(key):], uint32(right))
	binary.BigEndian.PutUint16(root.Page[nodepage.SlotArrayStart:], uint16(start))
	binary.BigEndian.PutUint16(root.Page[nodepage.SlotArrayStart+2:], uint16(len(key)+nodepage.InternalSuffix))
	root.SetFreeStart(nodepage.SlotArrayStart + nodepage.SlotSize)
	root.SetFreeEnd(uint16(start))
	root.SetItemCount(1)
	if err := root.Validate(); err != nil {
		t.Fatalf("the crafted root does not validate: %v", err)
	}
	for _, n := range []nodepage.Node{l, r, root} {
		if err := mem.Write(n.Page); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := Open(newPool(t, mem, 4, bufpool.LRU, bufpool.WriteBack), rootID)
	if err != nil {
		t.Fatal(err)
	}
	var perr error
	for i := 0; i < 400 && perr == nil; i++ {
		perr = tr.Put(k8(i), ridOf(i))
	}
	if !errors.Is(perr, ErrCorrupt) {
		t.Fatalf("a split whose parent cannot be split gave %v, want ErrCorrupt", perr)
	}
	noPins(t, tr, "the refused split")
}

type readBudget struct {
	bufpool.Source
	left int
}

func (s *readBudget) Read(id core.PageID) (page.Page, error) {
	if s.left == 0 {
		return nil, errInjected
	}
	s.left--
	return s.Source.Read(id)
}

func TestAScanOverAChainThatLoopsToAMiddleLeafStops(t *testing.T) {
	d := buildOnDisk(t, 2000, true)
	l := d.leaves()
	n := d.node(l[len(l)-1])
	n.SetNext(l[1])
	d.put(n)
	src := &readBudget{Source: d.src, left: 2 * d.src.Len()}
	tr, err := Open(newPool(t, src, 5, bufpool.LRU, bufpool.WriteBack), d.root)
	if err != nil {
		t.Fatal(err)
	}
	it := tr.Scan(nil, nil)
	if it.Next() || !errors.Is(it.Err(), ErrCorrupt) {
		t.Fatalf("scan over empty leaves looping to a middle leaf gave %v, want ErrCorrupt before reading any page twice", it.Err())
	}
	noPins(t, tr, "the looping scan")
}

func TestValidateCatchesAnInternalKeyEqualToItsLowBoundOverEmptyLeaves(t *testing.T) {
	d := buildOnDisk(t, 2000, true)
	root := d.node(d.root)
	mid := readImage(d.node(root.Child(1)))
	mid.entries[0].key = root.Key(0)
	p, err := mid.render()
	if err != nil {
		t.Fatal(err)
	}
	d.put(nodepage.Node{Page: p})
	tr, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Validate(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Validate gave %v", err)
	}
}

func TestValidateNeverFetchesASharedPageTwice(t *testing.T) {
	d := buildOnDisk(t, 2000, true)
	root := d.node(d.root)
	mid := readImage(d.node(root.Child(0)))
	mid.entries = nil
	p, err := mid.render()
	if err != nil {
		t.Fatal(err)
	}
	d.put(nodepage.Node{Page: p})
	for i := 1; i < root.ChildCount(); i++ {
		if err := root.SetChild(i, mid.id); err != nil {
			t.Fatal(err)
		}
	}
	d.put(root)
	tr, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	before := fetches(tr)
	if err := tr.Validate(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Validate gave %v", err)
	}
	if got := fetches(tr) - before; got > 3 {
		t.Fatalf("Validate fetched %d pages; only 3 distinct pages are reachable", got)
	}
}

func TestALoopingScanReadsEachLeafOnceBeforeReportingTheLoop(t *testing.T) {
	d := buildOnDisk(t, 2000, true)
	l := d.leaves()
	n := d.node(l[len(l)-1])
	n.SetNext(l[0])
	d.put(n)
	tr, err := d.open()
	if err != nil {
		t.Fatal(err)
	}
	h, err := tr.Height()
	if err != nil {
		t.Fatal(err)
	}
	before := fetches(tr)
	it := tr.Scan(nil, nil)
	if it.Next() || !errors.Is(it.Err(), ErrCorrupt) {
		t.Fatalf("scan gave %v", it.Err())
	}
	if got, want := fetches(tr)-before, uint64(h-1+len(l)); got != want {
		t.Fatalf("the looping scan fetched %d pages, want %d: one descent, then each leaf once", got, want)
	}
}
