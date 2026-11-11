package disktree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

var errInjected = errors.New("injected failure")

type faultSource struct {
	inner     bufpool.Source
	armReads  bool
	readsDead bool
	failAlloc int
	failRead  int
	failWrite int
	allocs    int
	reads     int
	writes    int
	chance    func() bool
}

func (s *faultSource) trip(counter *int, at int) bool {
	*counter++
	if at > 0 && *counter == at {
		return true
	}
	return s.chance != nil && s.chance()
}

func (s *faultSource) Read(id core.PageID) (page.Page, error) {
	if s.readsDead || s.trip(&s.reads, s.failRead) {
		return nil, errInjected
	}
	return s.inner.Read(id)
}

func (s *faultSource) Write(p page.Page) error {
	if s.trip(&s.writes, s.failWrite) {
		return errInjected
	}
	return s.inner.Write(p)
}

func (s *faultSource) Allocate(kind page.Kind) (page.Page, error) {
	if s.trip(&s.allocs, s.failAlloc) {
		return nil, errInjected
	}
	if s.armReads {
		s.readsDead = true
	}
	return s.inner.Allocate(kind)
}

func (s *faultSource) Free(id core.PageID) error { return s.inner.Free(id) }

func newPool(t testing.TB, src bufpool.Source, frames int, pol bufpool.Policy, mode bufpool.Mode) *bufpool.Pool {
	t.Helper()
	p, err := bufpool.New(src, bufpool.Options{Frames: frames, Policy: pol, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newTree(t testing.TB, frames int) (*Tree, *bufpool.MemSource) {
	t.Helper()
	src := bufpool.NewMemSource()
	tr, _, err := Create(newPool(t, src, frames, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	return tr, src
}

func k8(i int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(i))
	return b
}

func ridOf(i int) core.RowID {
	return core.RowID{Page: core.PageID(i/100 + 1), Slot: uint16(i % 100)}
}

func noPins(t testing.TB, tr *Tree, what string) {
	t.Helper()
	if n := tr.Pool().Pinned(); n != 0 {
		t.Fatalf("%s left %d frames pinned", what, n)
	}
}

func mustPut(t testing.TB, tr *Tree, key []byte, rid core.RowID) {
	t.Helper()
	if err := tr.Put(key, rid); err != nil {
		t.Fatalf("Put(% 02X): %v", key, err)
	}
}

func mustValidate(t testing.TB, tr *Tree) Shape {
	t.Helper()
	s, err := tr.Shape()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	noPins(t, tr, "Validate")
	return s
}

type kv struct {
	key []byte
	rid core.RowID
}

func collect(t testing.TB, it *Iterator) []kv {
	t.Helper()
	var out []kv
	for it.Next() {
		if n := len(out); n > 0 && bytes.Compare(out[n-1].key, it.Key()) >= 0 {
			t.Fatalf("scan returned % 02X after % 02X; keys must strictly increase", it.Key(), out[n-1].key)
		}
		out = append(out, kv{key: append([]byte(nil), it.Key()...), rid: it.RowID()})
	}
	if err := it.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func expectRange(model map[string]core.RowID, from, to []byte) []kv {
	var out []kv
	for k, v := range model {
		kb := []byte(k)
		if from != nil && bytes.Compare(kb, from) < 0 {
			continue
		}
		if to != nil && bytes.Compare(kb, to) >= 0 {
			continue
		}
		out = append(out, kv{key: kb, rid: v})
	}
	sort.Slice(out, func(a, b int) bool { return bytes.Compare(out[a].key, out[b].key) < 0 })
	return out
}

func sameKVs(got, want []kv) error {
	if len(got) != len(want) {
		return fmt.Errorf("got %d entries, want %d", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i].key, want[i].key) || got[i].rid != want[i].rid {
			return fmt.Errorf("entry %d is (% 02X, %+v), want (% 02X, %+v)",
				i, got[i].key, got[i].rid, want[i].key, want[i].rid)
		}
	}
	return nil
}

func checkModel(t testing.TB, tr *Tree, model map[string]core.RowID) {
	t.Helper()
	if err := sameKVs(collect(t, tr.Scan(nil, nil)), expectRange(model, nil, nil)); err != nil {
		t.Fatalf("full scan: %v", err)
	}
	for k, v := range model {
		got, ok, err := tr.Get([]byte(k))
		if err != nil || !ok || got != v {
			t.Fatalf("Get(% 02X) = %+v %v %v, want %+v", k, got, ok, err, v)
		}
	}
	noPins(t, tr, "checkModel")
}

func drain(t testing.TB, it *Iterator, limit int) int {
	t.Helper()
	var last []byte
	n := 0
	for it.Next() {
		if last != nil && bytes.Compare(last, it.Key()) >= 0 {
			t.Fatalf("scan returned % 02X after % 02X; keys must strictly increase", it.Key(), last)
		}
		last = append(last[:0], it.Key()...)
		n++
		if n > limit {
			t.Fatalf("scan returned more than %d keys", limit)
		}
	}
	return n
}
