package bufpool

import (
	"errors"
	"sync"
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

type probeLock struct {
	sync.Mutex
	name     string
	locks    int
	onUnlock func(name string)
}

func (l *probeLock) Lock() {
	l.Mutex.Lock()
	l.locks++
}

func (l *probeLock) Unlock() {
	if l.onUnlock != nil {
		l.onUnlock(l.name)
	}
	l.Mutex.Unlock()
}

type sliceLock []int

func (sliceLock) Lock()   {}
func (sliceLock) Unlock() {}

type chanLock chan struct{}

func (c chanLock) Lock()   { c <- struct{}{} }
func (c chanLock) Unlock() { <-c }

type taggedLock struct {
	tag any
	mu  *sync.Mutex
}

func (l taggedLock) Lock()   { l.mu.Lock() }
func (l taggedLock) Unlock() { l.mu.Unlock() }

func TestAnMTRLetsGoOfALockItTookOnlyAfterItsLatchesPinsAndPagesAreSettled(t *testing.T) {
	for _, commit := range []bool{true, false} {
		src, ids := seeded(t, 1)
		p := mustPool(t, src, Options{Frames: 2})
		m := p.Begin()
		l := &probeLock{}
		must(t, m.TakeLock(l))
		f := mtrFetch(t, m, ids[0])
		must(t, m.WLatch(f))
		f.Page()[page.HeaderSize] = 0x77
		fresh, err := m.NewPage(page.KindHeap)
		must(t, err)
		if !m.HasLock(l) || l.TryLock() {
			t.Fatal("the MTR does not hold the lock it took")
		}
		type state struct {
			latched bool
			pins    int
			b       byte
			dirty   bool
			pages   int
		}
		var at []state
		l.onUnlock = func(string) {
			at = append(at, state{!latchFree(f), p.Pinned(), f.Page()[page.HeaderSize], f.Dirty(), src.Len()})
		}
		end := m.Commit
		want := state{false, 0, 0x77, true, 2}
		if !commit {
			end = m.Abort
			want = state{false, 0, 0, false, 1}
		}
		must(t, end())
		if len(at) != 1 || at[0] != want {
			t.Fatalf("commit %v: at unlock the MTR's state was %+v, want once %+v", commit, at, want)
		}
		if m.HasLock(l) || !l.Mutex.TryLock() {
			t.Fatalf("commit %v: the lock is still held after the MTR ended", commit)
		}
		l.Mutex.Unlock()
		if commit && !p.Contains(fresh.ID()) {
			t.Fatal("the committed new page is gone")
		}
	}
}

func TestTakingALockTwiceInOneMTRLocksItOnceAndLocksAreLetGoInReverseOrder(t *testing.T) {
	p := mustPool(t, NewMemSource(), Options{Frames: 1})
	m := p.Begin()
	var order []string
	record := func(name string) { order = append(order, name) }
	a := &probeLock{name: "a", onUnlock: record}
	b := &probeLock{name: "b", onUnlock: record}
	must(t, m.TakeLock(a))
	must(t, m.TakeLock(b))
	must(t, m.TakeLock(a))
	if a.locks != 1 || b.locks != 1 || !m.HasLock(a) || !m.HasLock(b) {
		t.Fatalf("locked a %d times and b %d times, want once each", a.locks, b.locks)
	}
	must(t, m.Commit())
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("locks were let go in order %v, want b then a", order)
	}
}

func TestTakeLockRefusesAFinishedMTRANilLockAndOneThatIsNotAPointer(t *testing.T) {
	p := mustPool(t, NewMemSource(), Options{Frames: 1})
	done := p.Begin()
	must(t, done.Commit())
	l := &probeLock{}
	if err := done.TakeLock(l); !errors.Is(err, ErrMTRDone) {
		t.Fatalf("TakeLock on a committed MTR gave %v, want ErrMTRDone", err)
	}
	if l.locks != 0 || done.HasLock(l) {
		t.Fatal("a committed MTR took a lock nothing will ever let go")
	}
	m := p.Begin()
	must(t, m.TakeLock(l))
	for _, bad := range []struct {
		name string
		l    sync.Locker
	}{{"nil", nil}, {"a slice", sliceLock{1}}} {
		if err := m.TakeLock(bad.l); !errors.Is(err, ErrBadLock) {
			t.Fatalf("TakeLock(%s) gave %v, want ErrBadLock", bad.name, err)
		}
		if m.HasLock(bad.l) {
			t.Fatalf("the MTR claims to hold %s", bad.name)
		}
	}
	must(t, m.Abort())
	if l.locks != 1 || !l.Mutex.TryLock() {
		t.Fatal("the aborted MTR did not let go of the one good lock")
	}
}

func TestLatchedTellsAPageTheMTRLatchedFromOneItOnlyPinned(t *testing.T) {
	src, ids := seeded(t, 4)
	p := mustPool(t, src, Options{Frames: 4})
	m := p.Begin()
	mtrFetch(t, m, ids[0])
	b := mtrFetch(t, m, ids[1])
	c := mtrFetch(t, m, ids[2])
	must(t, m.RLatch(b))
	must(t, m.WLatch(c))
	if m.Latched(ids[0]) || !m.Latched(ids[1]) || !m.Latched(ids[2]) || m.Latched(ids[3]) {
		t.Fatalf("Latched = %v %v %v %v, want false true true false",
			m.Latched(ids[0]), m.Latched(ids[1]), m.Latched(ids[2]), m.Latched(ids[3]))
	}
	must(t, m.Release(b))
	if m.Latched(ids[1]) {
		t.Fatal("a released page still counts as latched")
	}
	must(t, m.Commit())
	if m.Latched(ids[2]) {
		t.Fatal("a committed MTR still counts a page as latched")
	}
}

func TestTakeLockTakesOnlyANonNilPointerAndNeverLocksAnythingElse(t *testing.T) {
	p := mustPool(t, NewMemSource(), Options{Frames: 1})
	m := p.Begin()
	var mu sync.Mutex
	open := make(chanLock, 1)
	var nilMutex *sync.Mutex
	for _, bad := range []struct {
		name string
		l    sync.Locker
	}{
		{"a channel", open},
		{"a nil channel", chanLock(nil)},
		{"a struct value", taggedLock{tag: 1, mu: &mu}},
		{"a struct value holding a slice", taggedLock{tag: []int{1}, mu: &mu}},
		{"a nil *sync.Mutex", nilMutex},
	} {
		if err := takeLockCatchingPanics(m, bad.l); !errors.Is(err, ErrBadLock) {
			t.Errorf("TakeLock(%s) gave %v, want ErrBadLock", bad.name, err)
		}
	}
	if len(open) != 0 || !mu.TryLock() {
		t.Fatal("TakeLock locked a lock it refused")
	}
	mu.Unlock()
	var rw, shared sync.RWMutex
	for _, good := range []sync.Locker{&mu, &rw, shared.RLocker()} {
		must(t, m.TakeLock(good))
		if !m.HasLock(good) {
			t.Fatalf("the MTR does not hold %T", good)
		}
	}
	if mu.TryLock() || rw.TryLock() || shared.TryLock() {
		t.Fatal("a lock the MTR took is still free")
	}
	must(t, m.Commit())
	if !mu.TryLock() || !rw.TryLock() || !shared.TryLock() {
		t.Fatal("Commit did not let go of every lock")
	}
}
