package disktree

import (
	"bytes"
	"errors"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/scute-db/scutedb/internal/bufpool"
	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/nodepage"
)

const generous = 60 * time.Second

func waitUntilTheTreeLockIsTaken(tr *Tree) {
	for tr.mu.TryLock() {
		tr.mu.Unlock()
		runtime.Gosched()
	}
}

func treeLockFree(tr *Tree) bool {
	if !tr.mu.TryLock() {
		return false
	}
	tr.mu.Unlock()
	return true
}

func TestAnMTRThatChangesATreeTwiceNeverDeadlocksWithAnotherOperationOnThatTree(t *testing.T) {
	others := []struct {
		name string
		run  func(tr *Tree) error
		want func(model map[string]core.RowID)
	}{
		{"Get", func(tr *Tree) error { _, _, err := tr.Get(k8(3)); return err }, nil},
		{"Scan", func(tr *Tree) error {
			it := tr.Scan(nil, nil)
			for it.Next() {
			}
			return it.Err()
		}, nil},
		{"Validate", func(tr *Tree) error { return tr.Validate() }, nil},
		{"Height", func(tr *Tree) error { _, err := tr.Height(); return err }, nil},
		{"Put", func(tr *Tree) error { return tr.Put(k8(200), ridOf(200)) },
			func(model map[string]core.RowID) { model[string(k8(200))] = ridOf(200) }},
		{"Delete", func(tr *Tree) error { _, err := tr.Delete(k8(2)); return err },
			func(model map[string]core.RowID) { delete(model, string(k8(2))) }},
		{"PutMTR in its own MTR", func(tr *Tree) error {
			m := tr.Pool().Begin()
			if err := tr.PutMTR(m, k8(201), ridOf(201)); err != nil {
				return abort(m, err)
			}
			return m.Commit()
		}, func(model map[string]core.RowID) { model[string(k8(201))] = ridOf(201) }},
		{"DeleteMTR in its own MTR", func(tr *Tree) error {
			m := tr.Pool().Begin()
			if _, err := tr.DeleteMTR(m, k8(4)); err != nil {
				return abort(m, err)
			}
			return m.Commit()
		}, func(model map[string]core.RowID) { delete(model, string(k8(4))) }},
	}
	seconds := []struct {
		name string
		run  func(tr *Tree, m *bufpool.MTR) error
		want func(model map[string]core.RowID)
	}{
		{"PutMTR", func(tr *Tree, m *bufpool.MTR) error { return tr.PutMTR(m, k8(101), ridOf(101)) },
			func(model map[string]core.RowID) { model[string(k8(101))] = ridOf(101) }},
		{"DeleteMTR", func(tr *Tree, m *bufpool.MTR) error {
			ok, err := tr.DeleteMTR(m, k8(5))
			if err == nil && !ok {
				err = errors.New("DeleteMTR found no key 5")
			}
			return err
		}, func(model map[string]core.RowID) { delete(model, string(k8(5))) }},
	}
	for _, other := range others {
		for _, second := range seconds {
			t.Run(other.name+" then "+second.name, func(t *testing.T) {
				t.Parallel()
				tr, _ := newTree(t, 16)
				model := map[string]core.RowID{}
				for i := 0; i < 10; i++ {
					mustPut(t, tr, k8(i), ridOf(i))
					model[string(k8(i))] = ridOf(i)
				}
				m := tr.Pool().Begin()
				if err := tr.PutMTR(m, k8(100), ridOf(100)); err != nil {
					t.Fatal(err)
				}
				otherDone := make(chan error, 1)
				go func() { otherDone <- other.run(tr) }()
				waitUntilTheTreeLockIsTaken(tr)
				secondDone := make(chan error, 1)
				go func() { secondDone <- second.run(tr, m) }()
				select {
				case err := <-secondDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(generous):
					t.Fatalf("deadlock: the MTR still write-latches the leaf its first PutMTR changed, %s holds the tree lock "+
						"while it waits for that latch, and the MTR's %s waits for the tree lock", other.name, second.name)
				}
				if err := m.Commit(); err != nil {
					t.Fatal(err)
				}
				if err := <-otherDone; err != nil {
					t.Fatalf("%s: %v", other.name, err)
				}
				model[string(k8(100))] = ridOf(100)
				second.want(model)
				if other.want != nil {
					other.want(model)
				}
				mustValidate(t, tr)
				checkModel(t, tr, model)
			})
		}
	}
}

func TestAnMTRKeepsTheTreeLockFromItsFirstChangeUntilItCommitsOrAborts(t *testing.T) {
	calls := []struct {
		name string
		run  func(tr *Tree, m *bufpool.MTR) error
	}{
		{"PutMTR", func(tr *Tree, m *bufpool.MTR) error { return tr.PutMTR(m, k8(50), ridOf(50)) }},
		{"DeleteMTR", func(tr *Tree, m *bufpool.MTR) error { _, err := tr.DeleteMTR(m, k8(3)); return err }},
		{"DeleteMTR of a missing key", func(tr *Tree, m *bufpool.MTR) error { _, err := tr.DeleteMTR(m, k8(999)); return err }},
	}
	for _, c := range calls {
		for _, commit := range []bool{true, false} {
			tr, _ := newTree(t, 8)
			for i := 0; i < 10; i++ {
				mustPut(t, tr, k8(i), ridOf(i))
			}
			m := tr.Pool().Begin()
			if err := c.run(tr, m); err != nil {
				t.Fatal(err)
			}
			if tr.mu.TryRLock() {
				tr.mu.RUnlock()
				t.Fatalf("%s returned and let a reader take the tree lock while its MTR is still open", c.name)
			}
			if !m.HasLock(&tr.mu) {
				t.Fatalf("%s did not record the tree lock on its MTR", c.name)
			}
			end := m.Commit
			if !commit {
				end = m.Abort
			}
			if err := end(); err != nil {
				t.Fatal(err)
			}
			if !treeLockFree(tr) {
				t.Fatalf("%s: the tree lock is still taken after the MTR ended (commit %v)", c.name, commit)
			}
			noPins(t, tr, c.name)
		}
	}

	src := bufpool.NewMemSource()
	pool := newPool(t, src, 4, bufpool.LRU, bufpool.WriteBack)
	m := pool.Begin()
	tr, _, err := CreateMTR(m)
	if err != nil {
		t.Fatal(err)
	}
	if treeLockFree(tr) || !m.HasLock(&tr.mu) {
		t.Fatal("CreateMTR handed out a tree whose lock its MTR does not hold")
	}
	if err := tr.PutMTR(m, k8(1), ridOf(1)); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(); err != nil {
		t.Fatal(err)
	}
	if !treeLockFree(tr) {
		t.Fatal("the tree lock is still taken after the creating MTR committed")
	}
	if rid, ok, err := tr.Get(k8(1)); err != nil || !ok || rid != ridOf(1) {
		t.Fatalf("Get after a create and a put in one MTR = %+v %v %v", rid, ok, err)
	}
	noPins(t, tr, "CreateMTR")
}

func TestAMutatorThatRefusesItsMTRLeavesTheTreeLockFree(t *testing.T) {
	tr, _ := newTree(t, 8)
	mustPut(t, tr, k8(1), ridOf(1))
	done := tr.Pool().Begin()
	if err := done.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tr.PutMTR(done, k8(2), ridOf(2)); !errors.Is(err, bufpool.ErrMTRDone) {
		t.Fatalf("PutMTR with a committed MTR gave %v", err)
	}
	if _, err := tr.DeleteMTR(done, k8(1)); !errors.Is(err, bufpool.ErrMTRDone) {
		t.Fatalf("DeleteMTR with a committed MTR gave %v", err)
	}
	if _, _, err := CreateMTR(done); !errors.Is(err, bufpool.ErrMTRDone) {
		t.Fatalf("CreateMTR with a committed MTR gave %v", err)
	}
	fresh := tr.Pool().Begin()
	if err := tr.PutMTR(fresh, nil, ridOf(1)); !errors.Is(err, nodepage.ErrEmptyKey) {
		t.Fatalf("PutMTR of an empty key gave %v", err)
	}
	if fresh.HasLock(&tr.mu) || !treeLockFree(tr) {
		t.Fatal("a refused call took the tree lock and nothing will ever let it go")
	}
	if err := fresh.Abort(); err != nil {
		t.Fatal(err)
	}
	mustPut(t, tr, k8(2), ridOf(2))
	noPins(t, tr, "the refused calls")
}

func TestAnMTRThatOnlyPinnedAPageStillWaitsForItsLatchBeforeTheTreeReadsIt(t *testing.T) {
	tr, _ := newTree(t, 8)
	model := map[string]core.RowID{}
	for i := 0; i < 10; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
		model[string(k8(i))] = ridOf(i)
	}
	b := tr.Pool().Begin()
	root, err := b.Fetch(tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	a := tr.Pool().Begin()
	fa, err := a.Fetch(tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.WLatch(fa); err != nil {
		t.Fatal(err)
	}
	copy(fa.Page(), nodepage.NewLeaf(tr.Root()).Page)
	reached := make(chan struct{}, 1)
	tr.pause = func(stage string) {
		if stage == "leaf" {
			select {
			case reached <- struct{}{}:
			default:
			}
		}
	}
	bDone := make(chan error, 1)
	go func() { bDone <- tr.PutMTR(b, k8(5), ridOf(500)) }()
	deadline := time.Now().Add(generous)
	for root.Pins() < 3 {
		select {
		case <-reached:
			if err := a.Abort(); err != nil {
				t.Error(err)
			}
			<-bDone
			t.Fatal("PutMTR read the root its MTR had only pinned while another MTR held the root's write latch")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("PutMTR neither read the root nor waited for its latch")
		}
		runtime.Gosched()
	}
	if err := a.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("PutMTR after the other MTR put the root back gave %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	model[string(k8(5))] = ridOf(500)
	mustValidate(t, tr)
	checkModel(t, tr, model)
}

func TestAPutMTRWhoseMTROnlyPinsTheRootNeverSeesAnotherMTRsUncommittedSplit(t *testing.T) {
	tr, _ := newTree(t, 32)
	model := map[string]core.RowID{}
	for i := 0; i < 200; i++ {
		mustPut(t, tr, k8(i), ridOf(i))
		model[string(k8(i))] = ridOf(i)
	}
	var a *bufpool.MTR
	for j := 0; a == nil; j++ {
		m := tr.Pool().Begin()
		if err := tr.PutMTR(m, k8(1000+j), ridOf(j)); err != nil {
			t.Fatal(err)
		}
		if len(m.Modified()) > 1 {
			a = m
			break
		}
		if err := m.Commit(); err != nil {
			t.Fatal(err)
		}
		model[string(k8(1000+j))] = ridOf(j)
	}
	var fresh []*bufpool.Frame
	for _, id := range a.Modified() {
		if id == tr.Root() {
			continue
		}
		f, err := a.Fetch(id)
		if err != nil {
			t.Fatal(err)
		}
		fresh = append(fresh, f)
	}
	if len(fresh) != 2 || tr.Pool().Pinned() != 3 {
		t.Fatalf("the uncommitted root split changed %v and pins %d pages", a.Modified(), tr.Pool().Pinned())
	}
	treeLockOpen := tr.mu.TryRLock()
	if treeLockOpen {
		tr.mu.RUnlock()
	}
	b := tr.Pool().Begin()
	if _, err := b.Fetch(tr.Root()); err != nil {
		t.Fatal(err)
	}
	bDone := make(chan error, 1)
	go func() { bDone <- tr.PutMTR(b, k8(5000), ridOf(5000)) }()
	if treeLockOpen {
		deadline := time.Now().Add(generous)
		for fresh[0].Pins()+fresh[1].Pins() < 3 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
	}
	if err := a.Abort(); err != nil {
		t.Fatalf("aborting the root split: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("PutMTR by an MTR that only pinned the root, after the other MTR aborted its root split: %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	model[string(k8(5000))] = ridOf(5000)
	mustValidate(t, tr)
	checkModel(t, tr, model)
}

func TestWritersChangingSeveralKeysPerMTRAlongsideReadersEndInTheExactState(t *testing.T) {
	const writers, readers = 4, 3
	rounds := 600
	if testing.Short() {
		rounds = 80
	}
	src := bufpool.NewMemSource()
	pool := newPool(t, src, 20, bufpool.LRUK, bufpool.WriteBack)
	tr, _, err := Create(pool)
	if err != nil {
		t.Fatal(err)
	}
	models := make([]map[string]core.RowID, writers)
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		models[g] = map[string]core.RowID{}
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(40 + g)))
			model := models[g]
			for n := 0; n < rounds; n++ {
				m := tr.Pool().Begin()
				pending := map[string]*core.RowID{}
				ops := 2 + r.Intn(2)
				var err error
				for o := 0; o < ops && err == nil; o++ {
					k := rangeKey(g, r.Intn(800))
					_, had := model[string(k)]
					if p, ok := pending[string(k)]; ok {
						had = p != nil
					}
					if r.Intn(3) == 0 {
						var ok bool
						if ok, err = tr.DeleteMTR(m, k); err == nil && ok != had {
							t.Errorf("writer %d: DeleteMTR found %v, the model says %v", g, ok, had)
						}
						pending[string(k)] = nil
					} else {
						rid := ridOf(n*10 + o)
						err = tr.PutMTR(m, k, rid)
						pending[string(k)] = &rid
					}
				}
				if err != nil {
					t.Errorf("writer %d: %v", g, abort(m, err))
					return
				}
				if r.Intn(4) == 0 {
					if err := m.Abort(); err != nil {
						t.Errorf("writer %d: Abort: %v", g, err)
						return
					}
					continue
				}
				if err := m.Commit(); err != nil {
					t.Errorf("writer %d: Commit: %v", g, err)
					return
				}
				for k, p := range pending {
					if p == nil {
						delete(model, k)
					} else {
						model[k] = *p
					}
				}
			}
		}(g)
	}
	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for g := 0; g < readers; g++ {
		rwg.Add(1)
		go func(g int) {
			defer rwg.Done()
			r := rand.New(rand.NewSource(int64(80 + g)))
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				switch n % 3 {
				case 0:
					if err := tr.Validate(); err != nil {
						t.Errorf("reader %d: Validate: %v", g, err)
						return
					}
				case 1:
					if _, _, err := tr.Get(rangeKey(r.Intn(writers), r.Intn(800))); err != nil {
						t.Errorf("reader %d: Get: %v", g, err)
						return
					}
				default:
					it := tr.Scan(nil, nil)
					var last []byte
					for it.Next() {
						if last != nil && bytes.Compare(last, it.Key()) >= 0 {
							t.Errorf("reader %d: scan went from % 02X to % 02X", g, last, it.Key())
							return
						}
						last = append(last[:0], it.Key()...)
					}
					if err := it.Err(); err != nil {
						t.Errorf("reader %d: Scan: %v", g, err)
						return
					}
				}
			}
		}(g)
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(generous):
		t.Fatal("deadlock: writers holding an MTR across several tree calls never finished")
	}
	close(stop)
	rwg.Wait()
	if t.Failed() {
		return
	}
	all := map[string]core.RowID{}
	for _, model := range models {
		for k, v := range model {
			all[k] = v
		}
	}
	s := mustValidate(t, tr)
	checkModel(t, tr, all)
	if s.Keys != len(all) {
		t.Fatalf("the tree holds %d keys, the writers' models %d", s.Keys, len(all))
	}
	t.Logf("height %d, %d keys, %d pages, %d evictions", s.Height, s.Keys, s.Pages(), pool.Stats().Evictions)
}
