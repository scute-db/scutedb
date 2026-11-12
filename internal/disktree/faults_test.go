package disktree

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func TestAFailedAllocationDuringASplitLeavesTheTreeUnchanged(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		mem := bufpool.NewMemSource()
		fs := &faultSource{inner: mem}
		pool := newPool(t, fs, 6, bufpool.LRU, bufpool.WriteBack)
		tr, _, err := Create(pool)
		if err != nil {
			t.Fatal(err)
		}
		model := map[string]core.RowID{}
		big := func(i int) []byte { return bytes.Repeat([]byte{byte(i >> 8), byte(i)}, nodepage.MaxKeySize/2) }
		for i := 0; i < 64; i++ {
			mustPut(t, tr, big(i), ridOf(i))
			model[string(big(i))] = ridOf(i)
		}
		before := mustValidate(t, tr)

		fs.failAt(failAt)
		failed := false
		for i := 64; i < 400; i++ {
			pagesBefore := mem.Len()
			err := tr.Put(big(i), ridOf(i))
			noPins(t, tr, "a failed Put")
			if err != nil {
				if !errors.Is(err, errInjected) {
					t.Fatalf("failAt %d: Put gave %v", failAt, err)
				}
				if mem.Len() != pagesBefore {
					t.Fatalf("failAt %d: the failed split leaked %d pages", failAt, mem.Len()-pagesBefore)
				}
				failed = true
				break
			}
			model[string(big(i))] = ridOf(i)
		}
		if !failed {
			t.Fatalf("failAt %d: no allocation ever failed", failAt)
		}
		after := mustValidate(t, tr)
		checkModel(t, tr, model)
		if after.Keys < before.Keys {
			t.Fatalf("failAt %d: keys went from %d to %d", failAt, before.Keys, after.Keys)
		}
		fs.failAlloc = 0
		for i := 400; i < 450; i++ {
			mustPut(t, tr, big(i), ridOf(i))
			model[string(big(i))] = ridOf(i)
		}
		mustValidate(t, tr)
		checkModel(t, tr, model)
	}
}

func (s *faultSource) failAt(n int) {
	s.allocs = 0
	s.failAlloc = n
}

func TestRandomDiskFailuresNeverLeavePinsOrPanic(t *testing.T) {
	steps := 20000
	if testing.Short() {
		steps = 3000
	}
	for seed := int64(0); seed < 3; seed++ {
		r := rand.New(rand.NewSource(seed))
		fs := &faultSource{inner: bufpool.NewMemSource()}
		pool := newPool(t, fs, 4+int(seed), bufpool.Policy(seed), bufpool.Mode(seed%2))
		tr, _, err := Create(pool)
		if err != nil {
			t.Fatal(err)
		}
		fs.chance = func() bool { return r.Intn(100) == 0 }
		failures := 0
		for i := 0; i < steps; i++ {
			k := make([]byte, 1+r.Intn(200))
			r.Read(k[:1])
			var err error
			switch r.Intn(5) {
			case 0, 1:
				err = tr.Put(k, ridOf(i))
			case 2:
				_, err = tr.Delete(k)
			case 3:
				_, _, err = tr.Get(k)
			case 4:
				it := tr.Scan(k[:1], nil)
				for n := 0; n < 50 && it.Next(); n++ {
				}
				err = it.Err()
			}
			if err != nil {
				failures++
			}
			noPins(t, tr, "an operation on a failing disk")
		}
		fs.chance = nil
		mustValidate(t, tr)
		drain(t, tr.Scan(nil, nil), 1_000_000)
		noPins(t, tr, "the final scan")
		if failures == 0 {
			t.Fatalf("seed %d: no operation failed; the test injected nothing", seed)
		}
	}
}

func TestEveryAllocationReadAndWriteFailurePointUnpins(t *testing.T) {
	build := func() (*Tree, *faultSource) {
		fs := &faultSource{inner: bufpool.NewMemSource()}
		pool := newPool(t, fs, 4, bufpool.LRU, bufpool.WriteBack)
		tr, _, err := Create(pool)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3000; i++ {
			mustPut(t, tr, k8(i*2), ridOf(i))
		}
		fs.allocs, fs.reads, fs.writes = 0, 0, 0
		return tr, fs
	}
	for kind := 0; kind < 3; kind++ {
		for at := 1; at <= 12; at++ {
			tr, fs := build()
			switch kind {
			case 0:
				fs.failAlloc = at
			case 1:
				fs.failRead = at
			case 2:
				fs.failWrite = at
			}
			for i := 0; i < 400; i++ {
				_ = tr.Put(k8(i*2+1), ridOf(i))
				noPins(t, tr, "a Put through a failure point")
			}
			fs.failAlloc, fs.failRead, fs.failWrite = 0, 0, 0
			mustValidate(t, tr)
		}
	}
}

func TestAReadFailureAfterASplitAllocatedFreesThePageAndLeavesTheTreeUnchanged(t *testing.T) {
	mem := bufpool.NewMemSource()
	fs := &faultSource{inner: mem}
	pool := newPool(t, fs, 2, bufpool.LRU, bufpool.WriteBack)
	tr, _, err := Create(pool)
	if err != nil {
		t.Fatal(err)
	}
	model := map[string]core.RowID{}
	for i := 0; i < 1000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
		model[string(k8(i))] = ridOf(i)
	}
	if h, _ := tr.Height(); h != 2 {
		t.Fatalf("height %d, want 2", h)
	}
	pages := mem.Len()
	fs.armReads = true
	var failed bool
	for i := 1000; i < 2000; i++ {
		err := tr.Put(k8(i), ridOf(i))
		noPins(t, tr, "Put")
		if err != nil {
			if !errors.Is(err, errInjected) {
				t.Fatalf("Put gave %v", err)
			}
			failed = true
			break
		}
		model[string(k8(i))] = ridOf(i)
		pages = mem.Len()
	}
	if !failed {
		t.Fatal("no split ever had to read its parent back")
	}
	fs.armReads, fs.readsDead = false, false
	if mem.Len() != pages {
		t.Fatalf("the failed split left %d new pages allocated", mem.Len()-pages)
	}
	mustValidate(t, tr)
	checkModel(t, tr, model)
}

func TestCreateFailsCleanlyWhenThePoolOrDiskSaysNo(t *testing.T) {
	src := bufpool.NewMemSource()
	pool := newPool(t, src, 1, bufpool.LRU, bufpool.WriteBack)
	f, err := pool.NewPage(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Create(pool); !errors.Is(err, bufpool.ErrNoFreeFrame) {
		t.Fatalf("Create with every frame pinned gave %v", err)
	}
	if err := pool.Unpin(f, false); err != nil {
		t.Fatal(err)
	}

	fs := &faultSource{inner: bufpool.NewMemSource(), failWrite: 1}
	wt := newPool(t, fs, 2, bufpool.LRU, bufpool.WriteThrough)
	if _, _, err := Create(wt); !errors.Is(err, errInjected) {
		t.Fatalf("Create through a write-through pool whose write fails gave %v", err)
	}
	if wt.Pinned() != 0 {
		t.Fatal("a failed Create left a pin")
	}
}
