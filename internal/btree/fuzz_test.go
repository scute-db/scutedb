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
