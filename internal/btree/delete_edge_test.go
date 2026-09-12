package btree

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

func separators(n *node, out *[][]byte) {
	if n.leaf {
		return
	}
	*out = append(*out, n.keys...)
	for _, c := range n.children {
		separators(c, out)
	}
}

func TestDeletingASeparatorKey(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 200; i++ {
		tr.Put(key(i), rid(i))
	}
	var seps [][]byte
	separators(tr.root, &seps)
	if len(seps) == 0 {
		t.Fatal("no separators to test")
	}
	target := seps[len(seps)/2]
	t.Logf("deleting a key that is also a separator in an internal node")
	if !tr.Delete(target) {
		t.Fatal("delete of a separator key failed")
	}
	if err := tr.Validate(); err != nil {
		t.Errorf("invariants broken after deleting a separator: %v", err)
	}
	if _, ok := tr.Get(target); ok {
		t.Error("the deleted separator key is still findable")
	}
	var still [][]byte
	separators(tr.root, &still)
	found := false
	for _, s := range still {
		if string(s) == string(target) {
			found = true
		}
	}
	t.Logf("the separator itself still present in an internal node: %v", found)
	for i := 0; i < 200; i++ {
		want := string(key(i)) != string(target)
		if _, ok := tr.Get(key(i)); ok != want {
			t.Errorf("Get(%d) = %v, want %v", i, ok, want)
		}
	}
}

func TestDeleteTwiceIsNotAnError(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 50; i++ {
		tr.Put(key(i), rid(i))
	}
	if !tr.Delete(key(10)) {
		t.Fatal("first delete failed")
	}
	if tr.Delete(key(10)) {
		t.Error("second delete of the same key reported success")
	}
	if tr.Len() != 49 {
		t.Errorf("Len = %d after one real and one failed delete, want 49", tr.Len())
	}
}

func TestDeleteDuringIterationKeepsTheTreeValid(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 200; i++ {
		tr.Put(key(i), rid(i))
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("delete during iteration panicked: %v", r)
		}
	}()
	seen := 0
	it := tr.ScanAll()
	for it.Next() {
		seen++
		if seen == 5 {
			for i := 100; i < 180; i++ {
				tr.Delete(key(i))
			}
		}
	}
	t.Logf("deleted 80 keys mid-scan; the iterator saw %d rows", seen)
	if err := tr.Validate(); err != nil {
		t.Errorf("tree broken by delete during iteration: %v", err)
	}
	if tr.Len() != 120 {
		t.Errorf("Len = %d, want 120", tr.Len())
	}
}

func TestAdapterSatisfiesTheInterface(t *testing.T) {
	x, err := NewIndex(8)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Put(key(1), rid(1)); err != nil {
		t.Fatal(err)
	}
	got, err := x.Get(key(1))
	if err != nil || got != rid(1) {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if _, err := x.Get(key(99)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Get of an absent key gave %v, want ErrNotFound", err)
	}
	if err := x.Delete(key(99)); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Delete of an absent key gave %v, want ErrNotFound", err)
	}
	if err := x.Delete(key(1)); err != nil {
		t.Errorf("Delete of a present key gave %v", err)
	}
	it, err := x.Scan(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for it.Next() {
		n++
	}
	if n != 0 {
		t.Errorf("scan of an emptied index returned %d rows", n)
	}
	if err := x.Close(); err != nil {
		t.Error(err)
	}
	if _, err := NewIndex(2); err == nil {
		t.Error("NewIndex(2) was accepted")
	}
}

func TestMinKeysHoldsForEveryOrder(t *testing.T) {
	for order := 3; order <= 40; order++ {
		tr := mustNew(t, order)
		for i := 0; i < 400; i++ {
			tr.Put(key(i), rid(i))
		}
		for i := 0; i < 400; i++ {
			tr.Delete(key(i))
			if err := tr.Validate(); err != nil {
				t.Fatalf("order %d, after deleting %d: %v", order, i, err)
			}
		}
	}
	fmt.Println("insert 400 then delete 400, invariants held for every order from 3 to 40")
}

func TestDeletePrefixAndEmptyKeys(t *testing.T) {
	tr := mustNew(t, 3)
	words := []string{"", "a", "aa", "aaa", "ab", "b", "ba", "bb"}
	for i, w := range words {
		tr.Put([]byte(w), rid(i))
	}
	if tr.Len() != len(words) {
		t.Fatalf("Len = %d, want %d", tr.Len(), len(words))
	}
	for _, w := range []string{"aa", "", "b"} {
		if !tr.Delete([]byte(w)) {
			t.Errorf("delete of %q failed", w)
		}
		if err := tr.Validate(); err != nil {
			t.Fatalf("after deleting %q: %v", w, err)
		}
	}
	gone := map[string]bool{"aa": true, "": true, "b": true}
	for _, w := range words {
		_, ok := tr.Get([]byte(w))
		if ok == gone[w] {
			t.Errorf("%q present=%v but deleted=%v", w, ok, gone[w])
		}
	}
	if tr.Delete(nil) {
		t.Error("Delete(nil) succeeded after the empty key was already removed")
	}
}

func TestSeparatorKeysNeedNotExist(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 100; i++ {
		tr.Put(key(i), rid(i))
	}
	var seps [][]byte
	separators(tr.root, &seps)
	for _, s := range seps {
		tr.Delete(s)
	}
	if err := tr.Validate(); err != nil {
		t.Errorf("deleting every separator broke invariants: %v", err)
	}
	var remaining [][]byte
	separators(tr.root, &remaining)
	ghosts := 0
	for _, s := range remaining {
		if _, ok := tr.Get(s); !ok {
			ghosts++
		}
	}
	t.Logf("%d of %d separators now name keys that no longer exist", ghosts, len(remaining))
	scanned := len(collect(tr.ScanAll()))
	if scanned != tr.Len() {
		t.Errorf("scan found %d keys but Len says %d", scanned, tr.Len())
	}
	for i := 0; i < 100; i++ {
		k := key(i)
		wasSep := false
		for _, s := range seps {
			if string(s) == string(k) {
				wasSep = true
			}
		}
		_, ok := tr.Get(k)
		if ok == wasSep {
			t.Errorf("key %d present=%v but was deleted=%v", i, ok, wasSep)
		}
	}
}

func TestValidateCatchesABadSeparator(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 50; i++ {
		tr.Put(key(i), rid(i))
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	if tr.root.leaf {
		t.Fatal("expected an internal root for this test")
	}

	saved := tr.root.keys[0]
	tr.root.keys[0] = key(0)
	if err := tr.Validate(); err == nil {
		t.Error("Validate accepted a separator that puts the left subtree out of bounds")
	}
	tr.root.keys[0] = saved

	last := len(tr.root.keys) - 1
	saved = tr.root.keys[last]
	tr.root.keys[last] = key(49)
	if err := tr.Validate(); err == nil {
		t.Error("Validate accepted a separator that puts the right subtree out of bounds")
	}
	tr.root.keys[last] = saved

	if err := tr.Validate(); err != nil {
		t.Fatalf("Validate broke after restoring the separators: %v", err)
	}
}

func TestIteratorSkipsSeveralEmptyLeavesInARow(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 40; i++ {
		tr.Put(key(i), rid(i))
	}
	want := len(collect(tr.ScanAll()))

	first := tr.firstLeaf()
	after := first.next
	e1 := &node{leaf: true}
	e2 := &node{leaf: true}
	e1.next = e2
	e2.next = after
	first.next = e1

	got := collect(tr.ScanAll())
	if len(got) != want {
		t.Fatalf("scan returned %d keys with two empty leaves spliced in, want %d",
			len(got), want)
	}
	for i := 1; i < len(got); i++ {
		if bytes.Compare(got[i-1], got[i]) >= 0 {
			t.Fatalf("scan not sorted at %d", i)
		}
	}
}
