package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

func key(n int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(n))
	return b
}

func rid(n int) core.RowID {
	return core.RowID{Page: core.PageID(n / 100), Slot: uint16(n % 100)}
}

func mustNew(t *testing.T, order int) *Tree {
	t.Helper()
	tr, err := New(order)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestOrderMustBeAtLeastThree(t *testing.T) {
	for _, o := range []int{-1, 0, 1, 2} {
		if _, err := New(o); !errors.Is(err, ErrBadOrder) {
			t.Fatalf("New(%d) gave %v, want ErrBadOrder", o, err)
		}
	}
	if _, err := New(3); err != nil {
		t.Fatalf("New(3) gave %v", err)
	}
}

func TestEmptyTree(t *testing.T) {
	tr := mustNew(t, 4)
	if _, ok := tr.Get(key(1)); ok {
		t.Fatal("empty tree returned a hit")
	}
	if tr.Len() != 0 || tr.Height() != 1 {
		t.Fatalf("Len=%d Height=%d, want 0 and 1", tr.Len(), tr.Height())
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 200; i++ {
		tr.Put(key(i), rid(i))
	}
	if tr.Len() != 200 {
		t.Fatalf("Len = %d, want 200", tr.Len())
	}
	for i := 0; i < 200; i++ {
		got, ok := tr.Get(key(i))
		if !ok || got != rid(i) {
			t.Fatalf("Get(%d) = %v, %v", i, got, ok)
		}
	}
	if _, ok := tr.Get(key(999)); ok {
		t.Fatal("found a key that was never inserted")
	}
}

func TestOverwriteDoesNotGrow(t *testing.T) {
	tr := mustNew(t, 4)
	tr.Put(key(1), rid(1))
	tr.Put(key(1), rid(2))
	if tr.Len() != 1 {
		t.Fatalf("Len = %d after overwriting the same key, want 1", tr.Len())
	}
	got, _ := tr.Get(key(1))
	if got != rid(2) {
		t.Fatalf("Get after overwrite = %v, want %v", got, rid(2))
	}
}

func TestKeysAreCopiedNotAliased(t *testing.T) {
	tr := mustNew(t, 4)
	buf := []byte("aaa")
	tr.Put(buf, rid(1))
	copy(buf, "zzz")
	if _, ok := tr.Get([]byte("aaa")); !ok {
		t.Fatal("mutating the caller's buffer changed the stored key")
	}
	if _, ok := tr.Get([]byte("zzz")); ok {
		t.Fatal("the tree stored a reference to the caller's buffer")
	}
}

func TestRootSplitsOnlyWhenFull(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 3; i++ {
		tr.Put(key(i), rid(i))
		if tr.Height() != 1 {
			t.Fatalf("after %d inserts height is %d, expected a single leaf", i+1, tr.Height())
		}
	}
	tr.Put(key(3), rid(3))
	if tr.Height() != 2 {
		t.Fatalf("after the 4th insert height is %d, want 2", tr.Height())
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestInvariantsHoldAfterEveryInsert(t *testing.T) {
	for _, order := range []int{3, 4, 5, 8, 32} {
		tr := mustNew(t, order)
		r := rand.New(rand.NewSource(int64(order)))
		seen := map[int]bool{}
		for i := 0; i < 500; i++ {
			k := r.Intn(2000)
			tr.Put(key(k), rid(k))
			seen[k] = true
			if err := tr.Validate(); err != nil {
				t.Fatalf("order %d, after %d inserts: %v", order, i+1, err)
			}
		}
		if tr.Len() != len(seen) {
			t.Fatalf("order %d: Len = %d, distinct keys = %d", order, tr.Len(), len(seen))
		}
		for k := range seen {
			if _, ok := tr.Get(key(k)); !ok {
				t.Fatalf("order %d: key %d went missing", order, k)
			}
		}
	}
}

func TestSortedAndReverseInsertionBothWork(t *testing.T) {
	for _, name := range []string{"ascending", "descending"} {
		tr := mustNew(t, 4)
		for i := 0; i < 300; i++ {
			n := i
			if name == "descending" {
				n = 299 - i
			}
			tr.Put(key(n), rid(n))
			if err := tr.Validate(); err != nil {
				t.Fatalf("%s, insert %d: %v", name, i, err)
			}
		}
		for i := 0; i < 300; i++ {
			if _, ok := tr.Get(key(i)); !ok {
				t.Fatalf("%s: key %d missing", name, i)
			}
		}
	}
}

func TestAllLeavesAtSameDepth(t *testing.T) {
	tr := mustNew(t, 3)
	for i := 0; i < 1000; i++ {
		tr.Put(key(i*7%1000), rid(i))
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestHeightGrowsLogarithmically(t *testing.T) {
	tr := mustNew(t, 64)
	heights := map[int]int{}
	for i := 0; i < 100000; i++ {
		tr.Put(key(i), rid(i))
		heights[tr.Height()] = i + 1
	}
	if h := tr.Height(); h > 4 {
		t.Fatalf("100k keys at order 64 reached height %d, expected 4 or fewer", h)
	}
	fmt.Printf("order 64, 100,000 keys -> height %d\n", tr.Height())
	for h := 1; h <= tr.Height(); h++ {
		if n, ok := heights[h]; ok {
			fmt.Printf("  height %d held up to %d keys\n", h, n)
		}
	}
}

func TestSeparatorKeysDirectSearchCorrectly(t *testing.T) {
	tr := mustNew(t, 3)
	for i := 0; i < 50; i++ {
		tr.Put(key(i), rid(i))
	}
	for i := 0; i < 50; i++ {
		k := key(i)
		n := tr.root
		for !n.leaf {
			n = n.children[n.childIndex(k)]
		}
		j := n.leafIndex(k)
		if j >= len(n.keys) || !bytes.Equal(n.keys[j], k) {
			t.Fatalf("descending for key %d landed on a leaf that does not contain it", i)
		}
	}
}
