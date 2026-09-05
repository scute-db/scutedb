package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func TestNilAndEmptyKeysAreTheSameKey(t *testing.T) {
	tr := mustNew(t, 4)
	tr.Put(nil, rid(1))
	tr.Put([]byte{}, rid(2))
	t.Logf("after Put(nil) then Put([]byte{}), Len = %d", tr.Len())
	got, ok := tr.Get(nil)
	t.Logf("Get(nil) -> %v %v", got, ok)
	if tr.Len() != 1 {
		t.Errorf("BUG: nil and empty key were treated as different keys")
	}
}

func TestPrefixKeys(t *testing.T) {
	tr := mustNew(t, 3)
	words := []string{"a", "aa", "aaa", "ab", "b", "", "ba"}
	for i, w := range words {
		tr.Put([]byte(w), rid(i))
		if err := tr.Validate(); err != nil {
			t.Fatalf("after %q: %v", w, err)
		}
	}
	for i, w := range words {
		got, ok := tr.Get([]byte(w))
		if !ok || got != rid(i) {
			t.Errorf("BUG: %q -> %v %v, want %v", w, got, ok, rid(i))
		}
	}
}

func leafKeysInOrder(n *node, out *[][]byte) {
	if n.leaf {
		*out = append(*out, n.keys...)
		return
	}
	for _, c := range n.children {
		leafKeysInOrder(c, out)
	}
}

func TestLeavesFormOneSortedSequence(t *testing.T) {
	for _, order := range []int{3, 4, 7, 16} {
		tr := mustNew(t, order)
		r := rand.New(rand.NewSource(int64(order) * 77))
		inserted := map[int]bool{}
		for i := 0; i < 2000; i++ {
			k := r.Intn(5000)
			tr.Put(key(k), rid(k))
			inserted[k] = true
		}
		var got [][]byte
		leafKeysInOrder(tr.root, &got)
		if len(got) != len(inserted) {
			t.Fatalf("order %d: leaves hold %d keys, inserted %d distinct",
				order, len(got), len(inserted))
		}
		for i := 1; i < len(got); i++ {
			if bytes.Compare(got[i-1], got[i]) >= 0 {
				t.Fatalf("order %d: leaf sequence not sorted at %d", order, i)
			}
		}
		for k := range inserted {
			v, ok := tr.Get(key(k))
			if !ok || v != rid(k) {
				t.Fatalf("order %d: key %d -> %v %v", order, k, v, ok)
			}
		}
	}
	fmt.Println("leaf sequences sorted and complete for orders 3, 4, 7, 16")
}

func TestNoPhantomKeys(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 500; i += 2 {
		tr.Put(key(i), rid(i))
	}
	for i := 1; i < 500; i += 2 {
		if _, ok := tr.Get(key(i)); ok {
			t.Fatalf("BUG: found key %d which was never inserted", i)
		}
	}
}

func TestOverwriteAcrossSplits(t *testing.T) {
	tr := mustNew(t, 3)
	for i := 0; i < 300; i++ {
		tr.Put(key(i), rid(i))
	}
	for i := 0; i < 300; i++ {
		tr.Put(key(i), rid(i+1000))
	}
	if tr.Len() != 300 {
		t.Fatalf("BUG: Len = %d after overwriting all 300 keys", tr.Len())
	}
	for i := 0; i < 300; i++ {
		got, _ := tr.Get(key(i))
		if got != rid(i+1000) {
			t.Fatalf("BUG: key %d kept the old value %v", i, got)
		}
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestZeroValueTreeIsUsable(t *testing.T) {
	var tr Tree
	if _, ok := tr.Get([]byte("x")); ok {
		t.Fatal("zero tree reported a hit")
	}
	if tr.Height() != 0 || tr.Len() != 0 {
		t.Fatalf("zero tree Height=%d Len=%d, want 0 and 0", tr.Height(), tr.Len())
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("zero tree Validate: %v", err)
	}
	if tr.Dump() != "" {
		t.Fatalf("zero tree Dump = %q", tr.Dump())
	}
	if tr.Order() != DefaultOrder {
		t.Fatalf("zero tree Order = %d, want %d", tr.Order(), DefaultOrder)
	}
	for i := 0; i < 500; i++ {
		tr.Put(key(i), rid(i))
	}
	if tr.Len() != 500 {
		t.Fatalf("Len = %d after 500 inserts into a zero tree", tr.Len())
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("after use: %v", err)
	}
	for i := 0; i < 500; i++ {
		if _, ok := tr.Get(key(i)); !ok {
			t.Fatalf("key %d missing", i)
		}
	}
}
