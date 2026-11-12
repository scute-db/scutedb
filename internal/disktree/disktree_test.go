package disktree

import (
	"bytes"
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func TestAnEmptyTreeFindsNothingAndIsValid(t *testing.T) {
	tr, _ := newTree(t, 4)
	if _, ok, err := tr.Get([]byte("x")); ok || err != nil {
		t.Fatalf("Get on an empty tree: %v %v", ok, err)
	}
	if ok, err := tr.Delete([]byte("x")); ok || err != nil {
		t.Fatalf("Delete on an empty tree: %v %v", ok, err)
	}
	if got := collect(t, tr.Scan(nil, nil)); len(got) != 0 {
		t.Fatalf("scan of an empty tree returned %d entries", len(got))
	}
	s := mustValidate(t, tr)
	if s.Height != 1 || s.Leaves != 1 || s.Keys != 0 || s.EmptyLeaves != 1 || s.Pages() != 1 || s.LeafFill() != 0 {
		t.Fatalf("empty tree shape %+v", s)
	}
	if h, err := tr.Height(); h != 1 || err != nil {
		t.Fatalf("Height %d %v", h, err)
	}
	noPins(t, tr, "empty tree")
}

func TestPutGetAndOverwrite(t *testing.T) {
	tr, _ := newTree(t, 4)
	mustPut(t, tr, []byte("apple"), ridOf(1))
	mustPut(t, tr, []byte("banana"), ridOf(2))
	mustPut(t, tr, []byte("apple"), ridOf(3))
	if rid, ok, err := tr.Get([]byte("apple")); !ok || err != nil || rid != ridOf(3) {
		t.Fatalf("Get(apple) = %+v %v %v, want the overwritten row %+v", rid, ok, err, ridOf(3))
	}
	if rid, ok, _ := tr.Get([]byte("banana")); !ok || rid != ridOf(2) {
		t.Fatalf("Get(banana) = %+v %v", rid, ok)
	}
	if _, ok, _ := tr.Get([]byte("cherry")); ok {
		t.Fatal("Get found a key that was never put")
	}
	if s := mustValidate(t, tr); s.Keys != 2 {
		t.Fatalf("overwrite added a key: %d keys", s.Keys)
	}
}

func TestTheRootPageNeverMovesAsTheTreeGrows(t *testing.T) {
	tr, src := newTree(t, 8)
	root := tr.Root()
	const n = 6000
	heights := map[int]bool{}
	model := map[string]core.RowID{}
	for i := 0; i < n; i++ {
		k := wideKey(i * 7919 % n)
		mustPut(t, tr, k, ridOf(i))
		model[string(k)] = ridOf(i)
		if i%250 == 0 {
			h, err := tr.Height()
			if err != nil {
				t.Fatal(err)
			}
			heights[h] = true
			if tr.Root() != root {
				t.Fatalf("root moved from %d to %d at height %d", root, tr.Root(), h)
			}
		}
	}
	s := mustValidate(t, tr)
	if s.Height < 3 || !heights[1] || !heights[2] || !heights[3] {
		t.Fatalf("tree reached height %d and passed through %v; the test needs two root splits", s.Height, heights)
	}
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(newPool(t, src, 8, bufpool.Clock, bufpool.WriteBack), root)
	if err != nil {
		t.Fatal(err)
	}
	if again.Root() != root {
		t.Fatal("reopen moved the root")
	}
	checkModel(t, again, model)
}

func TestDeleteNeverMergesSoNodesMayEmptyAndTheTreeStaysValid(t *testing.T) {
	tr, _ := newTree(t, 8)
	const n = 20000
	for i := 0; i < n; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	full := mustValidate(t, tr)
	for i := 0; i < n; i++ {
		if i%10 == 0 {
			continue
		}
		ok, err := tr.Delete(k8(i))
		if !ok || err != nil {
			t.Fatalf("Delete(%d) = %v %v", i, ok, err)
		}
	}
	thin := mustValidate(t, tr)
	if thin.Pages() != full.Pages() || thin.Height != full.Height {
		t.Fatalf("deleting 90%% changed the tree from %d pages height %d to %d pages height %d; lazy delete must not merge",
			full.Pages(), full.Height, thin.Pages(), thin.Height)
	}
	if thin.Keys != n/10 || thin.LeafFill() >= full.LeafFill()/5 {
		t.Fatalf("after deletes: %d keys, fill %.3f (was %.3f)", thin.Keys, thin.LeafFill(), full.LeafFill())
	}

	for i := 0; i < n; i += 10 {
		if ok, err := tr.Delete(k8(i)); !ok || err != nil {
			t.Fatalf("Delete(%d) = %v %v", i, ok, err)
		}
	}
	empty := mustValidate(t, tr)
	if empty.Keys != 0 || empty.EmptyLeaves != empty.Leaves || empty.Pages() != full.Pages() {
		t.Fatalf("after deleting everything: %+v", empty)
	}
	if got := collect(t, tr.Scan(nil, nil)); len(got) != 0 {
		t.Fatalf("a scan over %d empty leaves returned %d entries", empty.Leaves, len(got))
	}

	for i := n - 1; i >= 0; i -= 3 {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	refill := mustValidate(t, tr)
	if refill.Keys != (n+2)/3 {
		t.Fatalf("after refilling: %d keys, want %d", refill.Keys, (n+2)/3)
	}
	if ok, err := tr.Delete(k8(n + 5)); ok || err != nil {
		t.Fatalf("Delete of a missing key = %v %v", ok, err)
	}
}

func TestNodesSplitByBytesSoBigKeysMeanFewerPerNode(t *testing.T) {
	small, _ := newTree(t, 8)
	big, _ := newTree(t, MinFrames(5))
	for i := 0; i < 400; i++ {
		mustPut(t, small, k8(i), ridOf(i))
		k := bytes.Repeat([]byte{byte(i >> 8), byte(i)}, nodepage.MaxKeySize/2)
		mustPut(t, big, k, ridOf(i))
	}
	s, b := mustValidate(t, small), mustValidate(t, big)
	if s.Leaves != 3 {
		t.Fatalf("400 eight-byte keys inserted in order took %d leaves, want 3 (226 per leaf, split in half)", s.Leaves)
	}
	if b.Leaves < 100 || b.Height < 4 {
		t.Fatalf("400 keys of %d bytes took %d leaves and height %d; byte-based splits should give ~4 per leaf",
			nodepage.MaxKeySize, b.Leaves, b.Height)
	}
	if b.LeafFill() < 0.49 {
		t.Fatalf("big-key leaves are only %.2f full", b.LeafFill())
	}
}

func TestMixedKeySizesSplitIntoHalvesThatFit(t *testing.T) {
	tr, _ := newTree(t, MinFrames(4))
	model := map[string]core.RowID{}
	sizes := []int{1, 999, 3, 1000, 17, 500, 2, 750, 64, 1000}
	for i := 0; i < 3000; i++ {
		size := sizes[i%len(sizes)]
		k := make([]byte, size)
		for j := range k {
			k[j] = byte(i*31 + j*7)
		}
		k[0] = byte(i % 251)
		mustPut(t, tr, k, ridOf(i))
		model[string(k)] = ridOf(i)
		if i%250 == 0 {
			mustValidate(t, tr)
		}
	}
	mustValidate(t, tr)
	checkModel(t, tr, model)
}

func TestKeysThatAreEmptyOrTooLongAreRefused(t *testing.T) {
	tr, _ := newTree(t, 4)
	if err := tr.Put(nil, ridOf(1)); !errors.Is(err, nodepage.ErrEmptyKey) {
		t.Errorf("Put(nil) gave %v, want ErrEmptyKey", err)
	}
	long := bytes.Repeat([]byte{'x'}, nodepage.MaxKeySize+1)
	if err := tr.Put(long, ridOf(1)); !errors.Is(err, nodepage.ErrKeyTooLarge) {
		t.Errorf("Put of %d bytes gave %v, want ErrKeyTooLarge", len(long), err)
	}
	if _, ok, err := tr.Get(long); ok || err != nil {
		t.Errorf("Get of an oversized key: %v %v", ok, err)
	}
	exact := bytes.Repeat([]byte{'x'}, nodepage.MaxKeySize)
	mustPut(t, tr, exact, ridOf(2))
	if rid, ok, _ := tr.Get(exact); !ok || rid != ridOf(2) {
		t.Fatal("a key of exactly MaxKeySize did not round trip")
	}
	noPins(t, tr, "refused keys")
}

func TestScanIsHalfOpenAndHonoursNilBounds(t *testing.T) {
	tr, _ := newTree(t, 5)
	model := map[string]core.RowID{}
	for i := 0; i < 3000; i += 3 {
		mustPut(t, tr, k8(i), ridOf(i))
		model[string(k8(i))] = ridOf(i)
	}
	cases := []struct{ from, to []byte }{
		{nil, nil},
		{k8(300), k8(600)},
		{k8(301), k8(601)},
		{k8(0), k8(3)},
		{nil, k8(30)},
		{k8(2970), nil},
		{k8(600), k8(600)},
		{k8(900), k8(300)},
		{k8(5000), nil},
		{[]byte{}, nil},
		{nil, []byte{}},
		{[]byte{0}, []byte{0, 0, 0, 0, 0, 0, 0, 9}},
	}
	for _, c := range cases {
		got := collect(t, tr.Scan(c.from, c.to))
		if err := sameKVs(got, expectRange(model, c.from, c.to)); err != nil {
			t.Errorf("Scan(% 02X, % 02X): %v", c.from, c.to, err)
		}
	}
	noPins(t, tr, "scans")
}

func TestAnIteratorHoldsNoPinBetweenCallsAndSurvivesChanges(t *testing.T) {
	tr, _ := newTree(t, 4)
	ever := map[string]bool{}
	for i := 0; i < 2000; i += 2 {
		mustPut(t, tr, k8(i), ridOf(i))
		ever[string(k8(i))] = true
	}
	it := tr.Scan(nil, nil)
	var got [][]byte
	for it.Next() {
		noPins(t, tr, "an open iterator")
		k := append([]byte(nil), it.Key()...)
		if n := len(got); n > 0 && bytes.Compare(got[n-1], k) >= 0 {
			t.Fatalf("scan returned % 02X after % 02X", k, got[n-1])
		}
		got = append(got, k)
		i := int(k[7]) | int(k[6])<<8
		if i%100 == 0 {
			for j := 1; j < 200; j += 2 {
				mustPut(t, tr, k8(i+j), ridOf(i+j))
				ever[string(k8(i+j))] = true
			}
			if _, err := tr.Delete(k8(i + 1000)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, k := range got {
		if i > 0 && bytes.Compare(got[i-1], k) >= 0 {
			t.Fatalf("scan went backwards or repeated at %d: % 02X then % 02X", i, got[i-1], k)
		}
		if !ever[string(k)] {
			t.Fatalf("scan returned % 02X, which was never in the tree", k)
		}
		seen[string(k)] = true
	}
	for i := 0; i < 2000; i += 2 {
		deleted := i >= 1000 && i%100 == 0
		if !deleted && !seen[string(k8(i))] {
			t.Fatalf("key %d was in the tree for the whole scan and was not returned", i)
		}
	}
	if len(got) < 990+100 {
		t.Fatalf("scan returned only %d keys; odd keys inserted beyond the buffered leaf should show up", len(got))
	}
	mustValidate(t, tr)
	if it.Next() {
		t.Fatal("Next after the end returned true")
	}
}

func TestCloseStopsAnIterator(t *testing.T) {
	tr, _ := newTree(t, 4)
	for i := 0; i < 1000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	it := tr.Scan(nil, nil)
	if !it.Next() || !bytes.Equal(it.Key(), k8(0)) || it.RowID() != ridOf(0) {
		t.Fatal("first entry is wrong")
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	if it.Next() {
		t.Fatal("Next after Close returned true")
	}
	noPins(t, tr, "a closed iterator")
}

func TestOpenRefusesARootThatIsNotATreeNode(t *testing.T) {
	src := bufpool.NewMemSource()
	pool := newPool(t, src, 4, bufpool.LRU, bufpool.WriteBack)
	f, err := pool.NewPage(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	heap := f.ID()
	if err := pool.Unpin(f, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(pool, heap); !errors.Is(err, ErrCorrupt) || !errors.Is(err, nodepage.ErrBadKind) {
		t.Fatalf("Open on a heap page gave %v, want ErrCorrupt wrapping ErrBadKind", err)
	}
	if _, err := Open(pool, 999); !errors.Is(err, bufpool.ErrNoSuchPage) {
		t.Fatalf("Open on a missing page gave %v", err)
	}
	if _, err := Open(nil, 1); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Open(nil) gave %v", err)
	}
	if _, _, err := Create(nil); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Create(nil) gave %v", err)
	}
	if pool.Pinned() != 0 {
		t.Fatal("a refused Open left a pin")
	}
}

func TestTheIndexAdapterSpeaksTheIndexInterface(t *testing.T) {
	tr, _ := newTree(t, 4)
	x := NewIndex(tr)
	if x.Tree() != tr {
		t.Fatal("Tree() returned a different tree")
	}
	if _, err := x.Get([]byte("a")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get of a missing key gave %v", err)
	}
	if err := x.Put([]byte("a"), ridOf(1)); err != nil {
		t.Fatal(err)
	}
	if rid, err := x.Get([]byte("a")); err != nil || rid != ridOf(1) {
		t.Fatalf("Get = %+v %v", rid, err)
	}
	it, err := x.Scan(nil, nil)
	if err != nil || !it.Next() || string(it.Key()) != "a" || it.Next() || it.Err() != nil || it.Close() != nil {
		t.Fatal("adapter scan did not return exactly the one key")
	}
	if err := x.Delete([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := x.Delete([]byte("a")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second Delete gave %v", err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryPathUnpinsEvenWhenTheSourceFails(t *testing.T) {
	fs := &faultSource{inner: bufpool.NewMemSource()}
	pool := newPool(t, fs, 3, bufpool.LRU, bufpool.WriteBack)
	tr, _, err := Create(pool)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	x := NewIndex(tr)
	ops := []struct {
		name string
		run  func() error
	}{
		{"Get", func() error { _, _, err := tr.Get(k8(1500)); return err }},
		{"Put", func() error { return tr.Put(k8(99999), ridOf(1)) }},
		{"Delete", func() error { _, err := tr.Delete(k8(1)); return err }},
		{"Height", func() error { _, err := tr.Height(); return err }},
		{"Validate", tr.Validate},
		{"Scan", func() error { it := tr.Scan(nil, nil); it.Next(); return it.Err() }},
		{"index Get", func() error { _, err := x.Get(k8(7)); return err }},
		{"index Delete", func() error { return x.Delete(k8(7)) }},
	}
	for _, op := range ops {
		evictEverything(t, pool)
		fs.chance = func() bool { return true }
		if err := op.run(); !errors.Is(err, errInjected) {
			t.Errorf("%s with a failing disk gave %v", op.name, err)
		}
		fs.chance = nil
		noPins(t, tr, op.name)
	}
}

func evictEverything(t *testing.T, pool *bufpool.Pool) {
	t.Helper()
	if err := pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	var held []*bufpool.Frame
	for i := 0; i < pool.Frames(); i++ {
		f, err := pool.NewPage(page.KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	for _, f := range held {
		id := f.ID()
		if err := pool.Unpin(f, false); err != nil {
			t.Fatal(err)
		}
		if err := pool.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if pool.Resident() != 0 {
		t.Fatalf("%d pages still resident", pool.Resident())
	}
}
