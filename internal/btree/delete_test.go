package btree

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

func TestDeleteMissingKey(t *testing.T) {
	tr := mustNew(t, 4)
	if tr.Delete(key(1)) {
		t.Fatal("deleting from an empty tree reported success")
	}
	for i := 0; i < 10; i++ {
		tr.Put(key(i), rid(i))
	}
	if tr.Delete(key(99)) {
		t.Fatal("deleting an absent key reported success")
	}
	if tr.Len() != 10 {
		t.Fatalf("Len = %d after a failed delete, want 10", tr.Len())
	}
	var zero Tree
	if zero.Delete(key(1)) {
		t.Fatal("delete on a zero tree reported success")
	}
}

func TestDeleteOneKey(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 10; i++ {
		tr.Put(key(i), rid(i))
	}
	if !tr.Delete(key(5)) {
		t.Fatal("delete reported failure")
	}
	if tr.Len() != 9 {
		t.Fatalf("Len = %d, want 9", tr.Len())
	}
	if _, ok := tr.Get(key(5)); ok {
		t.Fatal("deleted key is still findable")
	}
	for _, i := range []int{0, 1, 2, 3, 4, 6, 7, 8, 9} {
		if _, ok := tr.Get(key(i)); !ok {
			t.Fatalf("key %d disappeared", i)
		}
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteEverythingLeavesAnEmptyTree(t *testing.T) {
	for _, order := range []int{3, 4, 7, 32} {
		tr := mustNew(t, order)
		for i := 0; i < 500; i++ {
			tr.Put(key(i), rid(i))
		}
		for i := 0; i < 500; i++ {
			if !tr.Delete(key(i)) {
				t.Fatalf("order %d: delete of %d failed", order, i)
			}
			if err := tr.Validate(); err != nil {
				t.Fatalf("order %d after deleting %d: %v", order, i, err)
			}
		}
		if tr.Len() != 0 {
			t.Fatalf("order %d: Len = %d after deleting everything", order, tr.Len())
		}
		if tr.Height() != 1 {
			t.Fatalf("order %d: height = %d after deleting everything, want 1", order, tr.Height())
		}
		if got := len(collect(tr.ScanAll())); got != 0 {
			t.Fatalf("order %d: scan found %d keys in an empty tree", order, got)
		}
		tr.Put(key(1), rid(1))
		if _, ok := tr.Get(key(1)); !ok {
			t.Fatalf("order %d: tree unusable after being emptied", order)
		}
	}
}

func TestDeleteInReverseOrder(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 400; i++ {
		tr.Put(key(i), rid(i))
	}
	for i := 399; i >= 0; i-- {
		if !tr.Delete(key(i)) {
			t.Fatalf("delete of %d failed", i)
		}
		if err := tr.Validate(); err != nil {
			t.Fatalf("after deleting %d: %v", i, err)
		}
	}
	if tr.Len() != 0 {
		t.Fatalf("Len = %d", tr.Len())
	}
}

func TestHeightShrinksBackDown(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 2000; i++ {
		tr.Put(key(i), rid(i))
	}
	tall := tr.Height()
	for i := 0; i < 1990; i++ {
		tr.Delete(key(i))
	}
	short := tr.Height()
	fmt.Printf("order 4: 2000 keys -> height %d, then 10 keys -> height %d (%d collapses)\n",
		tall, short, tr.Stats().Collapses)
	if short >= tall {
		t.Fatalf("height did not shrink: %d then %d", tall, short)
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryRepairPathGetsExercised(t *testing.T) {
	tr := mustNew(t, 4)
	r := rand.New(rand.NewSource(7))
	live := map[int]bool{}
	for i := 0; i < 3000; i++ {
		k := r.Intn(600)
		if live[k] && r.Intn(2) == 0 {
			tr.Delete(key(k))
			delete(live, k)
		} else {
			tr.Put(key(k), rid(k))
			live[k] = true
		}
		if err := tr.Validate(); err != nil {
			t.Fatalf("after operation %d: %v", i, err)
		}
	}
	for k := 0; k < 600; k++ {
		tr.Delete(key(k))
		if err := tr.Validate(); err != nil {
			t.Fatalf("draining, after deleting %d: %v", k, err)
		}
	}
	s := tr.Stats()
	fmt.Printf("3000 mixed operations then a full drain: %d borrows left, %d borrows right, %d merges, %d collapses\n",
		s.BorrowsLeft, s.BorrowsRight, s.Merges, s.Collapses)
	if s.BorrowsLeft == 0 {
		t.Error("borrow-from-left never happened; that path is untested")
	}
	if s.BorrowsRight == 0 {
		t.Error("borrow-from-right never happened; that path is untested")
	}
	if s.Merges == 0 {
		t.Error("merge never happened; that path is untested")
	}
	if s.Collapses == 0 {
		t.Error("root collapse never happened; that path is untested")
	}
}

func TestTreeMatchesAReferenceMapThroughout(t *testing.T) {
	for _, order := range []int{3, 4, 5, 8, 16} {
		tr := mustNew(t, order)
		want := map[int]bool{}
		r := rand.New(rand.NewSource(int64(order) * 31))

		for step := 0; step < 4000; step++ {
			k := r.Intn(800)
			if r.Intn(100) < 45 {
				gotOK := tr.Delete(key(k))
				wantOK := want[k]
				if gotOK != wantOK {
					t.Fatalf("order %d step %d: Delete(%d) = %v, want %v",
						order, step, k, gotOK, wantOK)
				}
				delete(want, k)
			} else {
				tr.Put(key(k), rid(k))
				want[k] = true
			}

			if err := tr.Validate(); err != nil {
				t.Fatalf("order %d step %d: %v", order, step, err)
			}
			if tr.Len() != len(want) {
				t.Fatalf("order %d step %d: Len = %d, reference has %d",
					order, step, tr.Len(), len(want))
			}
		}

		var expect []int
		for k := range want {
			expect = append(expect, k)
		}
		sort.Ints(expect)

		got := collect(tr.ScanAll())
		if len(got) != len(expect) {
			t.Fatalf("order %d: scan returned %d keys, reference has %d",
				order, len(got), len(expect))
		}
		for i := range got {
			if !bytes.Equal(got[i], key(expect[i])) {
				t.Fatalf("order %d: scan position %d differs from the reference", order, i)
			}
		}
		for _, k := range expect {
			if _, ok := tr.Get(key(k)); !ok {
				t.Fatalf("order %d: key %d in reference but missing from tree", order, k)
			}
		}
		for k := 0; k < 800; k++ {
			if _, ok := tr.Get(key(k)); ok != want[k] {
				t.Fatalf("order %d: Get(%d) = %v, reference says %v", order, k, ok, want[k])
			}
		}
	}
}

func TestScanStillWorksAfterDeletes(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 300; i++ {
		tr.Put(key(i), rid(i))
	}
	for i := 0; i < 300; i += 3 {
		tr.Delete(key(i))
	}
	got := collect(tr.Scan(key(50), key(100)))
	var want [][]byte
	for i := 50; i < 100; i++ {
		if i%3 != 0 {
			want = append(want, key(i))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("scan after deletes returned %d keys, want %d", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("position %d differs", i)
		}
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLeafChainSurvivesMerges(t *testing.T) {
	tr := mustNew(t, 3)
	for i := 0; i < 600; i++ {
		tr.Put(key(i), rid(i))
	}
	for i := 0; i < 600; i += 2 {
		tr.Delete(key(i))
		if err := tr.Validate(); err != nil {
			t.Fatalf("after deleting %d: %v", i, err)
		}
	}
	keys := 0
	for n := tr.firstLeaf(); n != nil; n = n.next {
		keys += len(n.keys)
	}
	if keys != tr.Len() {
		t.Fatalf("chain holds %d keys, tree has %d", keys, tr.Len())
	}
	if tr.Stats().Merges == 0 {
		t.Fatal("expected merges during this pattern")
	}
}
