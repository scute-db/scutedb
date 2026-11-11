package nodepage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"sort"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

type modelEntry struct {
	key   []byte
	rid   core.RowID
	child core.PageID
}

func checkAgainstModel(t *testing.T, n Node, first core.PageID, model []modelEntry) {
	t.Helper()
	if err := n.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if n.KeyCount() != len(model) {
		t.Fatalf("node holds %d keys, model holds %d", n.KeyCount(), len(model))
	}
	used := 0
	for i, e := range model {
		if !bytes.Equal(n.KeyRef(i), e.key) {
			t.Fatalf("key %d is % 02X, model says % 02X", i, n.KeyRef(i), e.key)
		}
		if n.Leaf() {
			if n.Row(i) != e.rid {
				t.Fatalf("row %d is %+v, model says %+v", i, n.Row(i), e.rid)
			}
		} else if n.Child(i+1) != e.child {
			t.Fatalf("child %d is %d, model says %d", i+1, n.Child(i+1), e.child)
		}
		used += n.EntrySize(len(e.key))
	}
	if !n.Leaf() && n.Child(0) != first {
		t.Fatalf("first child is %d, model says %d", n.Child(0), first)
	}
	if n.UsedBytes() != used {
		t.Fatalf("node says %d bytes used, entries add up to %d", n.UsedBytes(), used)
	}
	for i := int(n.FreeStart()); i < int(n.FreeEnd()); i++ {
		if n.Page[i] != 0 {
			t.Fatalf("free byte %d is %02X, want 00", i, n.Page[i])
		}
	}
}

func modelSearch(model []modelEntry, k []byte) (int, bool) {
	i := sort.Search(len(model), func(i int) bool { return bytes.Compare(model[i].key, k) >= 0 })
	return i, i < len(model) && bytes.Equal(model[i].key, k)
}

func randomKey(r *rand.Rand, max int) []byte {
	k := make([]byte, 1+r.Intn(max))
	r.Read(k)
	return k
}

func TestInsertLeafInAnyOrderKeepsKeysSortedAndTheNodeValid(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	n := NewLeaf(5)
	n.SetNext(6)
	var model []modelEntry
	for step := 0; step < 2000; step++ {
		k := randomKey(r, 24)
		i, found := n.Search(k)
		mi, mfound := modelSearch(model, k)
		if i != mi || found != mfound {
			t.Fatalf("Search(% 02X) = (%d, %v), model says (%d, %v)", k, i, found, mi, mfound)
		}
		if found {
			continue
		}
		rid := core.RowID{Page: core.PageID(step), Slot: uint16(step)}
		err := n.InsertLeaf(i, k, rid)
		if errors.Is(err, ErrNoFit) {
			break
		}
		if err != nil {
			t.Fatalf("InsertLeaf: %v", err)
		}
		model = append(model, modelEntry{})
		copy(model[i+1:], model[i:])
		model[i] = modelEntry{key: append([]byte(nil), k...), rid: rid}
		checkAgainstModel(t, n, 0, model)
	}
	if n.Next() != 6 {
		t.Fatalf("inserts changed the next link to %d", n.Next())
	}
	if len(model) < 100 {
		t.Fatalf("only %d keys went in before the node filled", len(model))
	}
}

func TestInsertRefusesWhatWouldBreakValidate(t *testing.T) {
	n := NewLeaf(1)
	for i, k := range []string{"b", "d", "f"} {
		if err := n.InsertLeaf(i, []byte(k), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := append([]byte(nil), n.Page...)
	cases := []struct {
		name string
		i    int
		key  string
		want error
	}{
		{"a key below its left neighbour", 1, "a", ErrOrder},
		{"a key equal to its left neighbour", 1, "b", ErrOrder},
		{"a key above its right neighbour", 1, "e", ErrOrder},
		{"a key equal to its right neighbour", 1, "d", ErrOrder},
		{"a negative index", -1, "a", ErrBadIndex},
		{"an index past the end", 4, "z", ErrBadIndex},
		{"an empty key", 0, "", ErrEmptyKey},
	}
	for _, c := range cases {
		if err := n.InsertLeaf(c.i, []byte(c.key), rid(9)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
		if !bytes.Equal(n.Page, before) {
			t.Fatalf("%s: a refused insert changed the page", c.name)
		}
	}
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMaxKeySizeGuaranteesFourEntriesPerNode(t *testing.T) {
	big := func(b byte) []byte { return bytes.Repeat([]byte{b}, MaxKeySize) }
	leaf := NewLeaf(1)
	internal, err := NewInternal(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := leaf.InsertLeaf(i, big(byte('a'+i)), rid(i)); err != nil {
			t.Fatalf("leaf entry %d of %d bytes: %v", i, MaxKeySize, err)
		}
		if err := internal.InsertInternal(i, big(byte('a'+i)), core.PageID(i)); err != nil {
			t.Fatalf("internal entry %d of %d bytes: %v", i, MaxKeySize, err)
		}
	}
	if err := leaf.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := internal.Validate(); err != nil {
		t.Fatal(err)
	}
	if MaxKeys(page.KindBTreeLeaf, MaxKeySize) < 4 || MaxKeys(page.KindBTreeInternal, MaxKeySize) < 4 {
		t.Fatalf("MaxKeys at the key limit is %d leaf, %d internal; want at least 4",
			MaxKeys(page.KindBTreeLeaf, MaxKeySize), MaxKeys(page.KindBTreeInternal, MaxKeySize))
	}

	tooBig := bytes.Repeat([]byte{'z'}, MaxKeySize+1)
	fresh := NewLeaf(3)
	if err := fresh.InsertLeaf(0, tooBig, rid(1)); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("InsertLeaf with %d bytes gave %v, want ErrKeyTooLarge", len(tooBig), err)
	}
	if err := fresh.AppendLeaf(tooBig, rid(1)); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("AppendLeaf with %d bytes gave %v, want ErrKeyTooLarge", len(tooBig), err)
	}
	if err := internal.InsertInternal(4, tooBig, 9); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("InsertInternal with %d bytes gave %v, want ErrKeyTooLarge", len(tooBig), err)
	}
	if err := fresh.AppendLeaf(nil, rid(1)); !errors.Is(err, ErrEmptyKey) {
		t.Errorf("AppendLeaf with an empty key gave %v, want ErrEmptyKey", err)
	}
	if err := CheckKey(big('q')); err != nil {
		t.Errorf("CheckKey refused a key of exactly MaxKeySize: %v", err)
	}
}

func TestRemoveCompactsSoFreedBytesAreReusable(t *testing.T) {
	n := NewLeaf(1)
	i := 0
	for n.InsertLeaf(i, key(i), rid(i)) == nil {
		i++
	}
	full := n.KeyCount()
	freeWhenFull := n.FreeSpace()

	for _, at := range []int{0, full / 2, full - 3} {
		if err := n.Remove(at); err != nil {
			t.Fatalf("Remove(%d): %v", at, err)
		}
		if err := n.Validate(); err != nil {
			t.Fatalf("Validate after Remove(%d): %v", at, err)
		}
	}
	if got, want := n.FreeSpace(), freeWhenFull+3*n.EntrySize(8); got != want {
		t.Fatalf("after three removals free space is %d, want %d", got, want)
	}

	added := 0
	for j := 0; j < 3; j++ {
		k := key(100000 + j)
		at, _ := n.Search(k)
		if err := n.InsertLeaf(at, k, rid(j)); err != nil {
			t.Fatalf("insert %d into freed space: %v", j, err)
		}
		added++
	}
	if n.KeyCount() != full || n.FreeSpace() != freeWhenFull {
		t.Fatalf("after refilling: %d keys and %d free, want %d and %d",
			n.KeyCount(), n.FreeSpace(), full, freeWhenFull)
	}
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRemovingEveryKeyLeavesAnEmptyValidNode(t *testing.T) {
	for _, fromFront := range []bool{true, false} {
		n := NewLeaf(1)
		for i := 0; i < 40; i++ {
			if err := n.InsertLeaf(i, key(i), rid(i)); err != nil {
				t.Fatal(err)
			}
		}
		for n.KeyCount() > 0 {
			at := 0
			if !fromFront {
				at = n.KeyCount() - 1
			}
			if err := n.Remove(at); err != nil {
				t.Fatal(err)
			}
			if err := n.Validate(); err != nil {
				t.Fatal(err)
			}
		}
		if int(n.FreeEnd()) != page.Size || n.FreeStart() != SlotArrayStart {
			t.Fatalf("empty node has free span [%d, %d), want [%d, %d)",
				n.FreeStart(), n.FreeEnd(), SlotArrayStart, page.Size)
		}
		fresh := NewLeaf(1)
		if !bytes.Equal(n.Page, fresh.Page) {
			t.Fatal("a node emptied by Remove is not byte-identical to a fresh one")
		}
	}
}

func TestRemoveRejectsBadIndexesAndCorruptSlots(t *testing.T) {
	n := NewLeaf(1)
	for i := 0; i < 3; i++ {
		if err := n.InsertLeaf(i, key(i), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []int{-1, 3, 100} {
		if err := n.Remove(at); !errors.Is(err, ErrBadIndex) {
			t.Errorf("Remove(%d) gave %v, want ErrBadIndex", at, err)
		}
	}
	binary.BigEndian.PutUint16(n.Page[n.slotOffset(1):], SlotArrayStart)
	if err := n.Remove(1); !errors.Is(err, ErrInvariant) {
		t.Errorf("Remove of a slot pointing into the slot array gave %v, want ErrInvariant", err)
	}
	binary.BigEndian.PutUint16(n.Page[n.slotOffset(1):], page.Size-2)
	if err := n.Remove(1); !errors.Is(err, ErrInvariant) {
		t.Errorf("Remove of a slot running past the page gave %v, want ErrInvariant", err)
	}
}

func TestInternalInsertRemoveAndSetChildKeepChildrenAligned(t *testing.T) {
	n, err := NewInternal(9, 2)
	if err != nil {
		t.Fatal(err)
	}
	n.SetFirstChild(100)
	for i, k := range []int{20, 40, 60} {
		if err := n.InsertInternal(i, key(k), core.PageID(101+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.InsertInternal(1, key(30), 200); err != nil {
		t.Fatal(err)
	}
	want := []modelEntry{{key: key(20), child: 101}, {key: key(30), child: 200}, {key: key(40), child: 102}, {key: key(60), child: 103}}
	checkAgainstModel(t, n, 100, want)
	if got := n.ChildFor(key(35)); got != 200 {
		t.Fatalf("ChildFor(35) = %d, want 200", got)
	}
	if got := n.ChildIndex(key(35)); got != 2 {
		t.Fatalf("ChildIndex(35) = %d, want 2", got)
	}

	if err := n.Remove(2); err != nil {
		t.Fatal(err)
	}
	want = []modelEntry{{key: key(20), child: 101}, {key: key(30), child: 200}, {key: key(60), child: 103}}
	checkAgainstModel(t, n, 100, want)

	if err := n.SetChild(0, 500); err != nil {
		t.Fatal(err)
	}
	if err := n.SetChild(3, 503); err != nil {
		t.Fatal(err)
	}
	want[2].child = 503
	checkAgainstModel(t, n, 500, want)

	for _, at := range []int{-1, 4} {
		if err := n.SetChild(at, 1); !errors.Is(err, ErrBadIndex) {
			t.Errorf("SetChild(%d) gave %v, want ErrBadIndex", at, err)
		}
	}
}

func TestSetRowOverwritesOnlyTheRowID(t *testing.T) {
	n := NewLeaf(1)
	for i := 0; i < 5; i++ {
		if err := n.InsertLeaf(i, key(i), rid(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := n.FreeSpace()
	if err := n.SetRow(2, core.RowID{Page: 0xDEADBEEF, Slot: 0xBEEF}); err != nil {
		t.Fatal(err)
	}
	if n.Row(2) != (core.RowID{Page: 0xDEADBEEF, Slot: 0xBEEF}) {
		t.Fatalf("row 2 is %+v after SetRow", n.Row(2))
	}
	for _, i := range []int{0, 1, 3, 4} {
		if n.Row(i) != rid(i) || !bytes.Equal(n.KeyRef(i), key(i)) {
			t.Fatalf("SetRow(2) disturbed entry %d", i)
		}
	}
	if !bytes.Equal(n.KeyRef(2), key(2)) || n.FreeSpace() != before {
		t.Fatal("SetRow changed the key or the free space")
	}
	for _, at := range []int{-1, 5} {
		if err := n.SetRow(at, rid(0)); !errors.Is(err, ErrBadIndex) {
			t.Errorf("SetRow(%d) gave %v, want ErrBadIndex", at, err)
		}
	}
	if err := n.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEditsRejectTheWrongKind(t *testing.T) {
	leaf := NewLeaf(1)
	internal, err := NewInternal(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.InsertInternal(0, key(1), 3); !errors.Is(err, ErrBadKind) {
		t.Errorf("InsertInternal on a leaf gave %v", err)
	}
	if err := internal.InsertLeaf(0, key(1), rid(1)); !errors.Is(err, ErrBadKind) {
		t.Errorf("InsertLeaf on an internal node gave %v", err)
	}
	if err := internal.SetRow(0, rid(1)); !errors.Is(err, ErrBadKind) {
		t.Errorf("SetRow on an internal node gave %v", err)
	}
	if err := leaf.SetChild(0, 3); !errors.Is(err, ErrBadKind) {
		t.Errorf("SetChild on a leaf gave %v", err)
	}
}

func TestRandomEditsMatchASortedSliceModel(t *testing.T) {
	for _, leaf := range []bool{true, false} {
		r := rand.New(rand.NewSource(42))
		var n Node
		if leaf {
			n = NewLeaf(1)
		} else {
			var err error
			n, err = NewInternal(1, 3)
			if err != nil {
				t.Fatal(err)
			}
			n.SetFirstChild(7)
		}
		first := core.PageID(7)
		var model []modelEntry
		for step := 0; step < 20000; step++ {
			switch op := r.Intn(10); {
			case op < 5:
				k := randomKey(r, 1+r.Intn(60))
				i, found := modelSearch(model, k)
				if found {
					continue
				}
				e := modelEntry{key: k, rid: core.RowID{Page: core.PageID(step), Slot: uint16(step)}, child: core.PageID(step)}
				var err error
				if leaf {
					err = n.InsertLeaf(i, k, e.rid)
				} else {
					err = n.InsertInternal(i, k, e.child)
				}
				if errors.Is(err, ErrNoFit) {
					if n.Fits(len(k)) {
						t.Fatalf("insert refused with ErrNoFit but Fits(%d) is true", len(k))
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				model = append(model, modelEntry{})
				copy(model[i+1:], model[i:])
				model[i] = e
			case op < 8 && len(model) > 0:
				i := r.Intn(len(model))
				if err := n.Remove(i); err != nil {
					t.Fatal(err)
				}
				model = append(model[:i], model[i+1:]...)
			case len(model) > 0:
				i := r.Intn(len(model))
				if leaf {
					rid := core.RowID{Page: core.PageID(r.Uint32()), Slot: uint16(r.Uint32())}
					if err := n.SetRow(i, rid); err != nil {
						t.Fatal(err)
					}
					model[i].rid = rid
				} else {
					c := r.Intn(len(model) + 1)
					id := core.PageID(r.Uint32())
					if err := n.SetChild(c, id); err != nil {
						t.Fatal(err)
					}
					if c == 0 {
						first = id
					} else {
						model[c-1].child = id
					}
				}
			}
			checkAgainstModel(t, n, first, model)
		}
	}
}
