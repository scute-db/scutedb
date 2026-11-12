package disktree

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
	"github.com/scute-db/scutedb/internal/page"
)

func TestMinFramesCoversTheDeepestSplit(t *testing.T) {
	for h, want := range map[int]int{-1: 3, 0: 3, 1: 3, 2: 5, 6: 13} {
		if got := MinFrames(h); got != want {
			t.Errorf("MinFrames(%d) = %d, want %d", h, got, want)
		}
	}
}

func bigKey(i int) []byte { return bytes.Repeat([]byte{byte(i >> 8), byte(i)}, nodepage.MaxKeySize/2) }

func TestAPoolTooSmallForASplitRefusesThePutAndLeavesTheTreeUnchanged(t *testing.T) {
	probe, _ := newTree(t, 64)
	k := 0
	for ; ; k++ {
		before := mustValidate(t, probe).Height
		mustPut(t, probe, bigKey(k), ridOf(k))
		if before == 3 && mustValidate(t, probe).Height == 4 {
			break
		}
	}
	src := bufpool.NewMemSource()
	tr, root, err := Create(newPool(t, src, 64, bufpool.LRU, bufpool.WriteBack))
	if err != nil {
		t.Fatal(err)
	}
	model := map[string]core.RowID{}
	for i := 0; i < k; i++ {
		mustPut(t, tr, bigKey(i), ridOf(i))
		model[string(bigKey(i))] = ridOf(i)
	}
	if err := tr.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	pages := src.Len()
	small, err := Open(newPool(t, src, MinFrames(3)-1, bufpool.LRU, bufpool.WriteBack), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := small.Put(bigKey(k), ridOf(k)); !errors.Is(err, bufpool.ErrNoFreeFrame) {
		t.Fatalf("a root split from height 3 through %d frames gave %v, want ErrNoFreeFrame", MinFrames(3)-1, err)
	}
	noPins(t, small, "the refused Put")
	if s := mustValidate(t, small); s.Height != 3 || src.Len() != pages {
		t.Fatalf("the refused split changed the tree: height %d, %d new pages", s.Height, src.Len()-pages)
	}
	checkModel(t, small, model)
	enough, err := Open(newPool(t, src, MinFrames(3), bufpool.LRU, bufpool.WriteBack), root)
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, enough, bigKey(k), ridOf(k))
	model[string(bigKey(k))] = ridOf(k)
	if s := mustValidate(t, enough); s.Height != 4 {
		t.Fatalf("with MinFrames(3) frames the root split gave height %d, want 4", s.Height)
	}
	checkModel(t, enough, model)
}

func TestTwoPutsInOneMTRCommitTogetherOrAbortTogether(t *testing.T) {
	for _, commit := range []bool{false, true} {
		tr, src := newTree(t, 16)
		model := map[string]core.RowID{}
		for i := 0; i < 226; i++ {
			mustPut(t, tr, k8(i), ridOf(i))
			model[string(k8(i))] = ridOf(i)
		}
		if err := tr.Pool().FlushAll(); err != nil {
			t.Fatal(err)
		}
		pages := src.Len()
		m := tr.Pool().Begin()
		for _, i := range []int{5000, 5001, 7} {
			if err := tr.PutMTR(m, k8(i), ridOf(i+1)); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := tr.DeleteMTR(m, k8(3)); !ok || err != nil {
			t.Fatalf("DeleteMTR = %v %v", ok, err)
		}
		if src.Len() != pages+2 || len(m.Modified()) != 3 {
			t.Fatalf("the MTR allocated %d pages and changed %v", src.Len()-pages, m.Modified())
		}
		if commit {
			if err := m.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, i := range []int{5000, 5001, 7} {
				model[string(k8(i))] = ridOf(i + 1)
			}
			delete(model, string(k8(3)))
		} else if err := m.Abort(); err != nil {
			t.Fatal(err)
		}
		s := mustValidate(t, tr)
		checkModel(t, tr, model)
		if s.Pages() != src.Len() || (!commit && (src.Len() != pages || s.Height != 1)) {
			t.Fatalf("commit %v: the tree owns %d pages, the source %d, height %d", commit, s.Pages(), src.Len(), s.Height)
		}
	}
}

func TestTreeMutatorsRefuseAMissingOrForeignMTR(t *testing.T) {
	tr, _ := newTree(t, 4)
	other := newPool(t, bufpool.NewMemSource(), 2, bufpool.LRU, bufpool.WriteBack)
	for _, c := range []struct {
		m    *bufpool.MTR
		want error
	}{{nil, ErrNoMTR}, {other.Begin(), ErrOtherPool}} {
		if err := tr.PutMTR(c.m, []byte("k"), ridOf(1)); !errors.Is(err, c.want) {
			t.Errorf("PutMTR gave %v, want %v", err, c.want)
		}
		if _, err := tr.DeleteMTR(c.m, []byte("k")); !errors.Is(err, c.want) {
			t.Errorf("DeleteMTR gave %v, want %v", err, c.want)
		}
	}
	if _, _, err := CreateMTR(nil); !errors.Is(err, ErrNoMTR) {
		t.Errorf("CreateMTR(nil) gave %v", err)
	}
	if err := tr.PutMTR(tr.Pool().Begin(), nil, ridOf(1)); !errors.Is(err, nodepage.ErrEmptyKey) {
		t.Errorf("PutMTR of an empty key gave %v", err)
	}
	noPins(t, tr, "refused calls")
}

func TestCreateMTRGivesItsPageBackOnAbort(t *testing.T) {
	src := bufpool.NewMemSource()
	pool := newPool(t, src, 2, bufpool.LRU, bufpool.WriteBack)
	m := pool.Begin()
	tr, root, err := CreateMTR(m)
	if err != nil || tr.Root() != root || !m.Holds(root) {
		t.Fatalf("CreateMTR = %v, root %d held %v", err, root, m.Holds(root))
	}
	if err := m.Abort(); err != nil {
		t.Fatal(err)
	}
	if src.Len() != 0 || pool.Pinned() != 0 {
		t.Fatalf("an aborted create left %d pages and %d pins", src.Len(), pool.Pinned())
	}
	full := newPool(t, src, 1, bufpool.LRU, bufpool.WriteBack)
	f, err := full.NewPage(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	m = full.Begin()
	if _, _, err := CreateMTR(m); !errors.Is(err, bufpool.ErrNoFreeFrame) {
		t.Fatalf("CreateMTR in a full pool gave %v", err)
	}
	if err := m.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := full.Unpin(f, false); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteMTRLetsGoOfTheLeafAtOnceWhenTheKeyIsMissing(t *testing.T) {
	tr, _ := newTree(t, 4)
	for i := 0; i < 300; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	m := tr.Pool().Begin()
	if ok, err := tr.DeleteMTR(m, k8(1000)); ok || err != nil {
		t.Fatalf("DeleteMTR of a missing key = %v %v", ok, err)
	}
	if m.Held() != 0 {
		t.Fatalf("a delete that changed nothing still holds %d pages", m.Held())
	}
	if ok, err := tr.DeleteMTR(m, k8(299)); !ok || err != nil {
		t.Fatalf("DeleteMTR = %v %v", ok, err)
	}
	if ok, err := tr.DeleteMTR(m, k8(299)); ok || err != nil {
		t.Fatalf("a second DeleteMTR of the key in the same MTR = %v %v", ok, err)
	}
	if m.Held() != 1 || len(m.Modified()) != 1 {
		t.Fatalf("after a delete the MTR holds %d pages, changed %d", m.Held(), len(m.Modified()))
	}
	if err := m.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := tr.Get(k8(299)); ok {
		t.Fatal("the committed delete did not stick")
	}
	noPins(t, tr, "DeleteMTR")
}

type nthSource struct {
	inner bufpool.Source
	n     int
	at    int
	what  string
}

func (s *nthSource) tick(what string) bool {
	s.n++
	if s.at > 0 && s.n == s.at {
		s.what = what
		return true
	}
	return false
}

func (s *nthSource) Read(id core.PageID) (page.Page, error) {
	if s.tick(fmt.Sprintf("read %d", id)) {
		return nil, errInjected
	}
	return s.inner.Read(id)
}

func (s *nthSource) Write(p page.Page) error {
	if s.tick(fmt.Sprintf("write %d", p.ID())) {
		return errInjected
	}
	return s.inner.Write(p)
}

func (s *nthSource) Allocate(kind page.Kind) (page.Page, error) {
	if s.tick("allocate") {
		return nil, errInjected
	}
	return s.inner.Allocate(kind)
}

func (s *nthSource) Free(id core.PageID) error { return s.inner.Free(id) }

type splitCase struct {
	name   string
	frames int
	key    func(int) []byte
	build  int
	grows  func(before, after Shape) bool
}

func evictAllBut(t *testing.T, tr *Tree, src *bufpool.MemSource, key []byte) {
	t.Helper()
	var path []step
	_, v, err := tr.descend(nil, key, &path)
	if err != nil {
		t.Fatal(err)
	}
	keep := map[core.PageID]bool{v.f.ID(): true}
	if err := tr.release(v, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range path {
		keep[s.id] = true
	}
	p := tr.Pool()
	if err := p.FlushAll(); err != nil {
		t.Fatal(err)
	}
	var held []*bufpool.Frame
	for id := core.PageID(src.Len()); len(held) < p.Frames() && id > 0; id-- {
		if keep[id] {
			continue
		}
		f, err := p.Fetch(id)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	for _, f := range held {
		if err := p.Unpin(f, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEveryFailurePointInsideOneSplitLeavesTheTreeExactlyAsItWas(t *testing.T) {
	cases := []splitCase{
		{"root leaf split", 3, k8, 226, func(b, a Shape) bool { return a.Height > b.Height }},
		{"leaf split under the root", 3, k8, 600, func(b, a Shape) bool { return a.Leaves > b.Leaves }},
		{"leaf and parent split", MinFrames(5), bigKey, 40, func(b, a Shape) bool { return a.Internal > b.Internal && a.Height == b.Height }},
		{"split up to a new root", MinFrames(5), bigKey, 40, func(b, a Shape) bool { return a.Height > b.Height }},
	}
	for _, c := range cases {
		for _, dirty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-dirty-%v", c.name, dirty), func(t *testing.T) {
				build := func() (*Tree, *nthSource, *bufpool.MemSource, map[string]core.RowID) {
					mem := bufpool.NewMemSource()
					src := &nthSource{inner: mem}
					tr, _, err := Create(newPool(t, src, c.frames, bufpool.LRU, bufpool.WriteBack))
					if err != nil {
						t.Fatal(err)
					}
					model := map[string]core.RowID{}
					for i := 0; i < c.build; i++ {
						mustPut(t, tr, c.key(2*i), ridOf(i))
						model[string(c.key(2*i))] = ridOf(i)
					}
					return tr, src, mem, model
				}
				key := -1
				probe, _, _, _ := build()
				for k := 0; key < 0 && k < 4*c.build; k++ {
					mid := mustValidate(t, probe)
					mustPut(t, probe, c.key(2*k+1), ridOf(1))
					if c.grows(mid, mustValidate(t, probe)) {
						key = k
					}
				}
				if key < 0 {
					t.Fatal("no key makes the split this case needs")
				}
				failures := 0
				for at := 1; ; at++ {
					tr, src, mem, model := build()
					for j := 1; j < 2*key+1; j += 2 {
						mustPut(t, tr, c.key(j), ridOf(j))
						model[string(c.key(j))] = ridOf(j)
					}
					evictAllBut(t, tr, mem, c.key(2*key+1))
					if dirty {
						for j := 0; j < 3; j++ {
							mustPut(t, tr, c.key(2*j), ridOf(7000+j))
							model[string(c.key(2*j))] = ridOf(7000 + j)
						}
					}
					before := mustValidate(t, tr)
					evictPages := mem.Len()
					src.n, src.at = 0, at
					err := tr.Put(c.key(2*key+1), ridOf(1))
					src.at = 0
					noPins(t, tr, "the cut-short split")
					if err == nil {
						if failures == 0 {
							t.Fatal("the split did no I/O at all")
						}
						t.Logf("%s, dirty %v: cut short at each of %d I/O points", c.name, dirty, failures)
						break
					}
					failures++
					if !errors.Is(err, errInjected) {
						t.Fatalf("failure %d (%s) gave %v", at, src.what, err)
					}
					after := mustValidate(t, tr)
					if after != before || mem.Len() != evictPages {
						t.Fatalf("failure at %s changed the tree: %+v -> %+v, %d pages -> %d", src.what, before, after, evictPages, mem.Len())
					}
					checkModel(t, tr, model)
					mustPut(t, tr, c.key(2*key+1), ridOf(1))
					model[string(c.key(2*key+1))] = ridOf(1)
					if s := mustValidate(t, tr); !c.grows(before, s) {
						t.Fatalf("after the disk healed the Put did not make the split: %+v -> %+v", before, s)
					}
					checkModel(t, tr, model)
				}
			})
		}
	}
}

func TestPagesASplitWritesAreTrustedWithoutCheckingThemAgain(t *testing.T) {
	tr, _ := newTree(t, 8)
	for i := 0; i < 227; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	for id := core.PageID(1); id <= 3; id++ {
		f, err := tr.Pool().Fetch(id)
		if err != nil {
			t.Fatal(err)
		}
		v := f.Verified()
		if err := tr.Pool().Unpin(f, false); err != nil {
			t.Fatal(err)
		}
		if !v {
			t.Fatalf("page %d was written by the split and is not marked verified", id)
		}
	}
	if h, _ := tr.Height(); h != 2 {
		t.Fatalf("height %d, want 2: the 227th key must split the root", h)
	}
}

func TestAScanStartedOnAOneLeafTreeReturnsExactKeysAcrossARootSplit(t *testing.T) {
	tr, _ := newTree(t, 8)
	for i := 0; i < 40; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	it := tr.Scan(nil, nil)
	if !it.Next() || !bytes.Equal(it.Key(), k8(0)) {
		t.Fatalf("first key %v %v", it.Key(), it.Err())
	}
	got := []kv{{key: k8(0), rid: it.RowID()}}
	for i := 40; i < 400; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	if h, _ := tr.Height(); h != 2 {
		t.Fatalf("height %d, want 2: the root must have split under the scan", h)
	}
	for it.Next() {
		got = append(got, kv{key: append([]byte(nil), it.Key()...), rid: it.RowID()})
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	want := make([]kv, 40)
	for i := range want {
		want[i] = kv{key: k8(i), rid: ridOf(i)}
	}
	if err := sameKVs(got, want); err != nil {
		t.Fatalf("the scan copied the one leaf it saw, so it must return exactly those 40 keys: %v", err)
	}
}

func TestAScanResumesAfterItsLastKeyAndSeesChangesMadeAheadOfIt(t *testing.T) {
	tr, _ := newTree(t, 8)
	for i := 0; i < 400; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	it := tr.Scan(nil, nil)
	if !it.Next() {
		t.Fatal(it.Err())
	}
	got := []kv{{key: k8(0), rid: it.RowID()}}
	for i := 1000; i < 2000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	for i := 300; i < 350; i++ {
		if ok, err := tr.Delete(k8(i)); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	mustPut(t, tr, k8(0), ridOf(5))
	for it.Next() {
		got = append(got, kv{key: append([]byte(nil), it.Key()...), rid: it.RowID()})
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	var want []kv
	for i := 0; i < 2000; i++ {
		if i < 300 || (i >= 350 && i < 400) || i >= 1000 {
			want = append(want, kv{key: k8(i), rid: ridOf(i)})
		}
	}
	if err := sameKVs(got, want); err != nil {
		t.Fatal(err)
	}
}

func TestAnIteratorHoldsNoPinLatchOrTreeLockBetweenNextCalls(t *testing.T) {
	tr, _ := newTree(t, 4)
	for i := 0; i < 1000; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	it := tr.Scan(nil, nil)
	var last []byte
	n := 0
	for it.Next() {
		if last != nil && bytes.Compare(last, it.Key()) >= 0 {
			t.Fatalf("scan returned % 02X after % 02X", it.Key(), last)
		}
		last = append(last[:0], it.Key()...)
		noPins(t, tr, "an open scan")
		if !tr.mu.TryLock() {
			t.Fatal("the scan holds the tree lock between Next calls")
		}
		tr.mu.Unlock()
		if n%100 == 0 {
			mustPut(t, tr, append(k8(n), 0), ridOf(n))
			if ok, err := tr.Delete(k8(n + 50)); !ok || err != nil {
				t.Fatalf("Delete during a scan = %v %v", ok, err)
			}
		}
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 990 {
		t.Fatalf("the scan returned only %d keys", n)
	}
	mustValidate(t, tr)
}

func TestMutatorsGivenAFinishedOrReadLatchingMTRFailAndChangeNothing(t *testing.T) {
	tr, src := newTree(t, 8)
	for i := 0; i < 226; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	done := tr.Pool().Begin()
	if err := done.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tr.PutMTR(done, k8(1), ridOf(9)); !errors.Is(err, bufpool.ErrMTRDone) {
		t.Fatalf("PutMTR with a committed MTR gave %v", err)
	}
	if _, err := tr.DeleteMTR(done, k8(1)); !errors.Is(err, bufpool.ErrMTRDone) {
		t.Fatalf("DeleteMTR with a committed MTR gave %v", err)
	}
	for _, key := range [][]byte{k8(3), k8(5000)} {
		m := tr.Pool().Begin()
		f, err := m.Fetch(tr.Root())
		if err != nil {
			t.Fatal(err)
		}
		if err := m.RLatch(f); err != nil {
			t.Fatal(err)
		}
		if err := tr.PutMTR(m, key, ridOf(9)); !errors.Is(err, bufpool.ErrUpgrade) {
			t.Fatalf("PutMTR(% 02X) while the MTR read-latches the root gave %v, want ErrUpgrade", key, err)
		}
		if err := m.Abort(); err != nil {
			t.Fatal(err)
		}
	}
	model := map[string]core.RowID{}
	for i := 0; i < 226; i++ {
		model[string(k8(i))] = ridOf(i)
	}
	checkModel(t, tr, model)
	if src.Len() != 1 {
		t.Fatalf("the refused calls left %d pages", src.Len())
	}
}

func TestALeafThatStopsBeingALeafBeforeItIsLatchedIsCorrupt(t *testing.T) {
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
	if err := tr.Put(k8(0), ridOf(1)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Put into a leaf that became an internal node gave %v, want ErrCorrupt", err)
	}
	noPins(t, tr, "the refused Put")
}

func TestASplitThatCannotReadItsPagesBackChangesNothing(t *testing.T) {
	for _, what := range []string{"parent", "leaf"} {
		mem := bufpool.NewMemSource()
		fs := &faultSource{inner: mem}
		tr, _, err := Create(newPool(t, fs, 3, bufpool.LRU, bufpool.WriteBack))
		if err != nil {
			t.Fatal(err)
		}
		model := map[string]core.RowID{}
		for i := 0; i < 1000; i++ {
			mustPut(t, tr, k8(2*i), ridOf(i))
			model[string(k8(2*i))] = ridOf(i)
		}
		pages := mem.Len()
		var current []byte
		fired := false
		tr.pause = onceAt("split", func() {
			fired = true
			if what == "leaf" {
				evictAllBut(t, tr, mem, current)
			} else {
				evictRoot(t, tr)
			}
			fs.readsDead = true
		})
		var putErr error
		for i := 0; i < 1000 && putErr == nil; i++ {
			current = k8(2*i + 1)
			if putErr = tr.Put(current, ridOf(i)); putErr == nil {
				model[string(current)] = ridOf(i)
				pages = mem.Len()
			}
		}
		fs.readsDead = false
		if !fired || !errors.Is(putErr, errInjected) {
			t.Fatalf("%s: a split that cannot read gave %v", what, putErr)
		}
		noPins(t, tr, "the failed split")
		if mem.Len() != pages {
			t.Fatalf("%s: the failed split left %d pages", what, mem.Len()-pages)
		}
		mustValidate(t, tr)
		checkModel(t, tr, model)
	}
}

type badPages struct {
	bufpool.Source
	bad map[core.PageID]bool
}

func (s *badPages) Write(p page.Page) error {
	if s.bad[p.ID()] {
		return errInjected
	}
	return s.Source.Write(p)
}

func TestAWriteThroughThatKeepsFailingIsReportedByEveryOperationThatLetsGoOfThePage(t *testing.T) {
	src := &badPages{Source: bufpool.NewMemSource(), bad: map[core.PageID]bool{}}
	pool := newPool(t, src, 8, bufpool.LRU, bufpool.WriteThrough)
	tr, root, err := Create(pool)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
	}
	leaf := leafOf(t, tr, k8(0))
	src.bad[leaf] = true
	if err := tr.Put(k8(0), ridOf(7)); !errors.Is(err, errInjected) {
		t.Fatalf("Put whose write-through fails gave %v", err)
	}
	checks := map[string]func() error{
		"Get":      func() error { _, _, err := tr.Get(k8(0)); return err },
		"Scan":     func() error { it := tr.Scan(nil, nil); it.Next(); return it.Err() },
		"Validate": tr.Validate,
		"Put":      func() error { return tr.Put(k8(1), ridOf(1)) },
		"Delete":   func() error { _, err := tr.Delete(k8(2)); return err },
	}
	for name, run := range checks {
		if err := run(); !errors.Is(err, errInjected) {
			t.Errorf("%s past a page whose write keeps failing gave %v", name, err)
		}
		noPins(t, tr, name)
	}
	src.bad[root] = true
	src.bad[leaf] = false
	m := pool.Begin()
	f, err := m.Fetch(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WLatch(f); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(); err != nil {
		t.Fatal(err)
	}
	f, err = pool.Fetch(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Unpin(f, true); !errors.Is(err, errInjected) {
		t.Fatalf("marking the root dirty gave %v", err)
	}
	if _, err := Open(pool, root); !errors.Is(err, errInjected) {
		t.Fatalf("Open past a root whose write keeps failing gave %v", err)
	}
}

func TestASplitUnderAnAncestorTheMTRReadLatchedIsRefused(t *testing.T) {
	tr, src := newTree(t, 8)
	for i := 0; i < 400; i++ {
		mustPut(t, tr, k8(2*i), ridOf(i))
	}
	model := map[string]core.RowID{}
	for i := 0; i < 400; i++ {
		model[string(k8(2*i))] = ridOf(i)
	}
	refused := false
	for i := 0; i < 400 && !refused; i++ {
		pages := src.Len()
		m := tr.Pool().Begin()
		f, err := m.Fetch(tr.Root())
		if err != nil {
			t.Fatal(err)
		}
		if err := m.RLatch(f); err != nil {
			t.Fatal(err)
		}
		err = tr.PutMTR(m, k8(2*i+1), ridOf(i))
		if err == nil {
			if err := m.Commit(); err != nil {
				t.Fatal(err)
			}
			model[string(k8(2*i+1))] = ridOf(i)
			continue
		}
		if !errors.Is(err, bufpool.ErrUpgrade) {
			t.Fatalf("a split under a read-latched root gave %v, want ErrUpgrade", err)
		}
		if err := m.Abort(); err != nil {
			t.Fatal(err)
		}
		if src.Len() != pages {
			t.Fatalf("the refused split left %d pages", src.Len()-pages)
		}
		refused = true
	}
	if !refused {
		t.Fatal("no Put needed to split")
	}
	mustValidate(t, tr)
	checkModel(t, tr, model)
}
