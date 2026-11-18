package disktree

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
)

func TestADeleteIntoALeafThatStopsBeingALeafIsCorruptAndLetsGoOfEverything(t *testing.T) {
	src := bufpool.NewMemSource()
	tr, _, err := Create(newPool(t, src, 3, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	leaf := leafOf(t, tr, k8(0))
	tr.pause = onceAt("leaf", func() {
		if err := tr.Pool().FlushAll(); err != nil {
			t.Fatal(err)
		}
		for id := core.PageID(1); tr.Pool().Contains(leaf); id++ {
			if id == leaf {
				continue
			}
			f, err := tr.Pool().Fetch(id)
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Pool().Unpin(f, false); err != nil {
				t.Fatal(err)
			}
		}
		n, err := nodepage.NewInternal(leaf, 1)
		if err != nil {
			t.Fatal(err)
		}
		n.SetFirstChild(leaf + 1)
		if err := src.Write(n.Page); err != nil {
			t.Fatal(err)
		}
	})
	if ok, err := tr.Delete(k8(0)); ok || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Delete from a leaf that became an internal node gave %v %v, want ErrCorrupt", ok, err)
	}
	noPins(t, tr, "the refused Delete")
}

func TestAScanThatFailedStaysFinishedAfterTheDiskRecovers(t *testing.T) {
	mem := bufpool.NewMemSource()
	fs := &faultSource{inner: mem}
	tr, _, err := Create(newPool(t, fs, 4, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	it := tr.Scan(nil, nil)
	n := 0
	for it.Next() {
		n++
		if n == 500 {
			fs.readsDead = true
		}
	}
	fs.readsDead = false
	if !errors.Is(it.Err(), errInjected) || n >= 3000 {
		t.Fatalf("the scan stopped after %d keys with %v, want the injected read failure", n, it.Err())
	}
	if it.Next() {
		t.Fatalf("Next after a failed refill returned % 02X once the disk recovered; a failed scan must stay finished", it.Key())
	}
	if !errors.Is(it.Err(), errInjected) {
		t.Fatalf("the scan's error changed to %v", it.Err())
	}
}

func TestASplitLatchesItsPagesTopDown(t *testing.T) {
	tr, src := newTree(t, 16)
	for i := 0; i < 600; i += 2 {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	if h, _ := tr.Height(); h != 2 {
		t.Fatalf("height %d, want 2", h)
	}
	for i := 1; i < 600; i += 2 {
		pages := src.Len()
		leaf := leafOf(t, tr, k8(i))
		m := tr.Pool().Begin()
		if err := tr.PutMTR(m, k8(i), ridOf(i)); err != nil {
			t.Fatal(err)
		}
		got := m.Modified()
		if err := m.Commit(); err != nil {
			t.Fatal(err)
		}
		if src.Len() == pages {
			continue
		}
		if len(got) != 3 || got[0] != tr.Root() || got[1] != leaf {
			t.Fatalf("a split of leaf %d under root %d latched %v, want the root, then the leaf, then the new page", leaf, tr.Root(), got)
		}
		return
	}
	t.Fatal("no Put split a leaf")
}

func TestEveryTreeOperationHoldsTheTreeLockInTheRightModeWhileItTouchesPages(t *testing.T) {
	ops := []struct {
		name   string
		writer bool
		run    func(tr *Tree) error
	}{
		{"Get", false, func(tr *Tree) error { _, _, err := tr.Get(k8(1)); return err }},
		{"Height", false, func(tr *Tree) error { _, err := tr.Height(); return err }},
		{"Validate", false, func(tr *Tree) error { return tr.Validate() }},
		{"Scan", false, func(tr *Tree) error { it := tr.Scan(nil, nil); it.Next(); return it.Err() }},
		{"Put", true, func(tr *Tree) error { return tr.Put(k8(100), ridOf(100)) }},
		{"Delete", true, func(tr *Tree) error { _, err := tr.Delete(k8(2)); return err }},
		{"PutMTR", true, func(tr *Tree) error {
			m := tr.Pool().Begin()
			if err := tr.PutMTR(m, k8(101), ridOf(101)); err != nil {
				return abort(m, err)
			}
			return m.Commit()
		}},
		{"DeleteMTR", true, func(tr *Tree) error {
			m := tr.Pool().Begin()
			if _, err := tr.DeleteMTR(m, k8(3)); err != nil {
				return abort(m, err)
			}
			return m.Commit()
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			tr, _ := newTree(t, 8)
			for i := 0; i < 10; i++ {
				mustPut(t, tr, k8(i), ridOf(i))
			}
			p := tr.Pool()
			root, err := p.Fetch(tr.Root())
			if err != nil {
				t.Fatal(err)
			}
			p.WLatch(root)
			done := make(chan error, 1)
			go func() { done <- op.run(tr) }()
			deadline := time.Now().Add(generous)
			for root.Pins() < 2 {
				if time.Now().After(deadline) {
					p.WUnlatch(root)
					t.Fatalf("%s never reached the root", op.name)
				}
				runtime.Gosched()
			}
			shared := tr.mu.TryRLock()
			if shared {
				tr.mu.RUnlock()
			}
			exclusive := tr.mu.TryLock()
			if exclusive {
				tr.mu.Unlock()
			}
			p.WUnlatch(root)
			if err := p.Unpin(root, false); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if exclusive {
				t.Fatalf("%s touched the root without holding the tree lock", op.name)
			}
			if op.writer && shared {
				t.Fatalf("%s is a writer but let a reader share the tree lock", op.name)
			}
			if !op.writer && !shared {
				t.Fatalf("%s is a reader but kept other readers out", op.name)
			}
			noPins(t, tr, op.name)
		})
	}
}
