package btree

import (
	"encoding/binary"
	"testing"
)

func TestScanCopiesTheUpperBound(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 100; i++ {
		tr.Put(key(i), rid(i))
	}
	to := make([]byte, 8)
	binary.BigEndian.PutUint64(to, 10)

	it := tr.Scan(key(0), to)
	n := 0
	for it.Next() {
		n++
		if n == 3 {
			binary.BigEndian.PutUint64(to, 90)
		}
	}
	t.Logf("scan [0,10) returned %d rows after the caller mutated 'to' mid-scan", n)
	if n != 10 {
		t.Errorf("Scan retains the caller's 'to' slice; the range changed mid-iteration")
	}
}

func TestScanDoesNotRetainTheLowerBound(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 100; i++ {
		tr.Put(key(i), rid(i))
	}
	from := make([]byte, 8)
	binary.BigEndian.PutUint64(from, 5)
	it := tr.Scan(from, key(15))
	binary.BigEndian.PutUint64(from, 99)
	n := 0
	for it.Next() {
		n++
	}
	t.Logf("scan [5,15) returned %d rows after mutating 'from' post-Scan", n)
	if n != 10 {
		t.Errorf("Scan retains the caller's 'from' slice")
	}
}

func TestKeyStaysValidAcrossNext(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 50; i++ {
		tr.Put(key(i), rid(i))
	}
	it := tr.ScanAll()
	it.Next()
	held := it.Key()
	first := binary.BigEndian.Uint64(held)
	it.Next()
	it.Next()
	after := binary.BigEndian.Uint64(held)
	t.Logf("key held across further Next calls: %d -> %d", first, after)
	if first != after {
		t.Errorf("Key() is invalidated by later Next calls")
	}
}

func TestInsertDuringIterationKeepsTheTreeValid(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 40; i += 2 {
		tr.Put(key(i), rid(i))
	}
	seen := 0
	it := tr.ScanAll()
	for it.Next() {
		seen++
		if seen == 3 {
			for i := 1; i < 40; i += 2 {
				tr.Put(key(i), rid(i))
			}
		}
	}
	t.Logf("inserted 20 keys mid-scan; the iterator saw %d rows total", seen)
	if err := tr.Validate(); err != nil {
		t.Errorf("tree invariants broken by insert during iteration: %v", err)
	}
	if got := len(collect(tr.ScanAll())); got != 40 {
		t.Errorf("tree holds %d keys after the interleaved inserts, want 40", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	tr := mustNew(t, 4)
	for i := 0; i < 20; i++ {
		tr.Put(key(i), rid(i))
	}
	it := tr.ScanAll()
	it.Next()
	it.Close()
	it.Close()
	if it.Next() {
		t.Errorf("Next returned true after two Closes")
	}
	if it.Err() != nil {
		t.Errorf("Err non-nil after Close: %v", it.Err())
	}
}
