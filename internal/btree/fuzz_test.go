package btree

import (
	"encoding/binary"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

func FuzzInsertKeepsInvariants(f *testing.F) {
	f.Add(4, []byte{1, 2, 3, 4, 5})
	f.Add(3, []byte{9, 8, 7, 6, 5, 4, 3, 2, 1})
	f.Add(5, []byte{1, 1, 1, 1})
	f.Add(64, []byte{})

	f.Fuzz(func(t *testing.T, order int, data []byte) {
		if order < MinOrder || order > 256 {
			return
		}
		if len(data) > 4096 {
			return
		}
		tr, err := New(order)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[byte]bool{}
		for _, b := range data {
			k := []byte{b}
			tr.Put(k, core.RowID{Slot: uint16(b)})
			seen[b] = true
			if err := tr.Validate(); err != nil {
				t.Fatalf("after inserting %d: %v", b, err)
			}
		}
		if tr.Len() != len(seen) {
			t.Fatalf("Len = %d, distinct bytes = %d", tr.Len(), len(seen))
		}
		for b := range seen {
			if _, ok := tr.Get([]byte{b}); !ok {
				t.Fatalf("key %d missing after %d inserts", b, len(data))
			}
		}
	})
}

func FuzzGetNeverPanics(f *testing.F) {
	f.Add(4, uint64(0), []byte{})
	f.Add(3, uint64(1<<63), []byte{0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, order int, seed uint64, probe []byte) {
		if order < MinOrder || order > 256 {
			return
		}
		tr, err := New(order)
		if err != nil {
			return
		}
		b := make([]byte, 8)
		for i := 0; i < 50; i++ {
			binary.BigEndian.PutUint64(b, seed+uint64(i)*2654435761)
			tr.Put(b, core.RowID{Slot: uint16(i)})
		}
		tr.Get(probe)
		tr.Get(nil)
		_ = tr.Height()
		_ = tr.Validate()
	})
}

func FuzzScanMatchesBruteForce(f *testing.F) {
	f.Add(4, []byte{1, 5, 3, 9, 2}, byte(2), byte(9))
	f.Add(3, []byte{}, byte(0), byte(255))
	f.Add(8, []byte{7, 7, 7}, byte(7), byte(7))

	f.Fuzz(func(t *testing.T, order int, data []byte, lo, hi byte) {
		if order < MinOrder || order > 128 || len(data) > 2048 {
			return
		}
		tr, err := New(order)
		if err != nil {
			return
		}
		present := map[byte]bool{}
		for _, b := range data {
			tr.Put([]byte{b}, core.RowID{Slot: uint16(b)})
			present[b] = true
		}

		var want []byte
		for b := 0; b < 256; b++ {
			if present[byte(b)] && byte(b) >= lo && byte(b) < hi {
				want = append(want, byte(b))
			}
		}

		var got []byte
		for it := tr.Scan([]byte{lo}, []byte{hi}); it.Next(); {
			k := it.Key()
			if len(k) != 1 {
				t.Fatalf("scan returned a %d-byte key", len(k))
			}
			got = append(got, k[0])
		}

		if len(got) != len(want) {
			t.Fatalf("scan [%d,%d) returned %d keys, brute force %d\n got  %v\n want %v",
				lo, hi, len(got), len(want), got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("position %d: got %d, want %d", i, got[i], want[i])
			}
		}
		if err := tr.Validate(); err != nil {
			t.Fatalf("invariants broken: %v", err)
		}
	})
}
