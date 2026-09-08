package btree

import (
	"bytes"
	"math/rand"
	"sort"
	"testing"
)

func collect(it *Iterator) [][]byte {
	var out [][]byte
	for it.Next() {
		out = append(out, append([]byte(nil), it.Key()...))
	}
	return out
}

func TestScanEmptyTree(t *testing.T) {
	tr := mustNew(t, 4)
	if got := collect(tr.ScanAll()); len(got) != 0 {
		t.Fatalf("empty tree scanned %d keys", len(got))
	}
	var zero Tree
	if got := collect(zero.ScanAll()); len(got) != 0 {
		t.Fatalf("zero tree scanned %d keys", len(got))
	}
}

func TestScanAllReturnsEverythingSorted(t *testing.T) {
	for _, order := range []int{3, 4, 8, 64} {
		tr := mustNew(t, order)
		r := rand.New(rand.NewSource(int64(order)))
		want := map[int]bool{}
		for i := 0; i < 1000; i++ {
			k := r.Intn(3000)
			tr.Put(key(k), rid(k))
			want[k] = true
		}
		got := collect(tr.ScanAll())
		if len(got) != len(want) {
			t.Fatalf("order %d: scanned %d keys, inserted %d distinct", order, len(got), len(want))
		}
		for i := 1; i < len(got); i++ {
			if bytes.Compare(got[i-1], got[i]) >= 0 {
				t.Fatalf("order %d: scan not sorted at position %d", order, i)
			}
		}
	}
}

func TestScanIsHalfOpen(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 20; i++ {
		tr.Put(key(i), rid(i))
	}
	got := collect(tr.Scan(key(5), key(10)))
	if len(got) != 5 {
		t.Fatalf("scan [5,10) returned %d keys, want 5", len(got))
	}
	if !bytes.Equal(got[0], key(5)) {
		t.Fatal("scan [5,10) did not include the lower bound")
	}
	if !bytes.Equal(got[len(got)-1], key(9)) {
		t.Fatal("scan [5,10) did not stop before the upper bound")
	}
}

func TestScanBoundsThatMatchNothing(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 10; i < 20; i++ {
		tr.Put(key(i), rid(i))
	}
	cases := []struct {
		name     string
		from, to []byte
		want     int
	}{
		{"entirely below", key(0), key(5), 0},
		{"entirely above", key(50), key(60), 0},
		{"empty range", key(15), key(15), 0},
		{"inverted", key(18), key(12), 0},
		{"from past the end", key(99), nil, 0},
		{"to before the start", nil, key(5), 0},
		{"open start", nil, key(13), 3},
		{"open end", key(17), nil, 3},
		{"both open", nil, nil, 10},
	}
	for _, c := range cases {
		if got := len(collect(tr.Scan(c.from, c.to))); got != c.want {
			t.Fatalf("%s: got %d keys, want %d", c.name, got, c.want)
		}
	}
}

func TestScanMatchesBruteForce(t *testing.T) {
	tr := mustNew(t, 5)
	r := rand.New(rand.NewSource(99))
	var all []int
	seen := map[int]bool{}
	for i := 0; i < 800; i++ {
		k := r.Intn(2000)
		tr.Put(key(k), rid(k))
		if !seen[k] {
			seen[k] = true
			all = append(all, k)
		}
	}
	sort.Ints(all)

	for trial := 0; trial < 300; trial++ {
		lo, hi := r.Intn(2000), r.Intn(2000)
		var want [][]byte
		for _, k := range all {
			if k >= lo && k < hi {
				want = append(want, key(k))
			}
		}
		got := collect(tr.Scan(key(lo), key(hi)))
		if len(got) != len(want) {
			t.Fatalf("[%d,%d): scan returned %d, brute force %d", lo, hi, len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("[%d,%d): position %d differs", lo, hi, i)
			}
		}
	}
}

func TestScanReturnsCorrectRowIDs(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 200; i++ {
		tr.Put(key(i), rid(i))
	}
	n := 0
	for it := tr.Scan(key(50), key(150)); it.Next(); {
		want := rid(50 + n)
		if it.RowID() != want {
			t.Fatalf("position %d: RowID %v, want %v", n, it.RowID(), want)
		}
		n++
	}
	if n != 100 {
		t.Fatalf("walked %d rows, want 100", n)
	}
}

func TestIteratorIsExhaustedForever(t *testing.T) {
	tr := mustNew(t, 4)
	tr.Put(key(1), rid(1))
	it := tr.ScanAll()
	if !it.Next() {
		t.Fatal("first Next returned false")
	}
	if it.Next() {
		t.Fatal("second Next returned true on a one-key tree")
	}
	for i := 0; i < 5; i++ {
		if it.Next() {
			t.Fatal("an exhausted iterator came back to life")
		}
	}
	if it.Err() != nil {
		t.Fatalf("Err = %v", it.Err())
	}
}

func TestCloseStopsIteration(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 100; i++ {
		tr.Put(key(i), rid(i))
	}
	it := tr.ScanAll()
	it.Next()
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	if it.Next() {
		t.Fatal("Next returned true after Close")
	}
}

func TestLeafChainIsAValidSequence(t *testing.T) {
	for _, order := range []int{3, 4, 16} {
		tr := mustNew(t, order)
		for i := 0; i < 2000; i++ {
			tr.Put(key(i*7919%2000), rid(i))
			if err := tr.Validate(); err != nil {
				t.Fatalf("order %d after %d inserts: %v", order, i+1, err)
			}
		}
		leaves, keys := 0, 0
		for n := tr.firstLeaf(); n != nil; n = n.next {
			leaves++
			keys += len(n.keys)
		}
		if keys != tr.Len() {
			t.Fatalf("order %d: chain holds %d keys, tree has %d", order, keys, tr.Len())
		}
		t.Logf("order %2d: %d keys across %d chained leaves, height %d",
			order, keys, leaves, tr.Height())
	}
}

func TestBrokenChainIsDetected(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 100; i++ {
		tr.Put(key(i), rid(i))
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	first := tr.firstLeaf()
	saved := first.next
	first.next = nil
	if err := tr.Validate(); err == nil {
		t.Fatal("Validate accepted a broken leaf chain")
	}
	first.next = saved

	first.next = first
	if err := tr.Validate(); err == nil {
		t.Fatal("Validate accepted a cyclic leaf chain")
	}
	first.next = saved
	if err := tr.Validate(); err != nil {
		t.Fatalf("Validate broke after restoring the chain: %v", err)
	}
}
