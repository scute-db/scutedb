package latch

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNewStripedRoundsUpToAPowerOfTwoAndStaysWithinTheCap(t *testing.T) {
	cases := map[int]int{-4: 1, 0: 1, 1: 1, 2: 2, 3: 4, 16: 16, 17: 32, MaxStripes: MaxStripes, MaxStripes + 5: MaxStripes}
	for n, want := range cases {
		if got := NewStriped(n).Stripes(); got != want {
			t.Errorf("NewStriped(%d) has %d stripes, want %d", n, got, want)
		}
	}
}

func TestEveryKeyMapsToOneStripeAndSequentialKeysSpreadEvenly(t *testing.T) {
	for _, n := range []int{1, 2, 16, 64} {
		s := NewStriped(n)
		count := make([]int, s.Stripes())
		for k := uint64(0); k < 4096; k++ {
			i := s.Index(k)
			if i < 0 || i >= s.Stripes() {
				t.Fatalf("%d stripes: key %d maps to stripe %d", n, k, i)
			}
			if s.For(k) != s.At(i) || s.Index(k) != i {
				t.Fatalf("%d stripes: key %d does not map to the same lock twice", n, k)
			}
			count[i]++
		}
		want := 4096 / s.Stripes()
		for i, c := range count {
			if c < want*3/4 || c > want*5/4 {
				t.Fatalf("%d stripes: stripe %d holds %d of 4096 sequential keys, want about %d", n, i, c, want)
			}
		}
	}
}

func TestAtWrapsAnyIndexOntoAStripe(t *testing.T) {
	s := NewStriped(8)
	if s.At(-1) != s.At(7) || s.At(8) != s.At(0) || s.At(13) != s.At(5) {
		t.Fatal("At does not wrap out-of-range indexes onto the stripes")
	}
}

func TestAStripeLockExcludesWritersAndSharesWithReaders(t *testing.T) {
	s := NewStriped(4)
	s.Lock(7)
	if s.For(7).TryRLock() {
		t.Fatal("a reader got in while the stripe was write-locked")
	}
	s.Unlock(7)
	s.RLock(7)
	if !s.For(7).TryRLock() {
		t.Fatal("a second reader was kept out of a read-locked stripe")
	}
	if s.For(7).TryLock() {
		t.Fatal("a writer got in while the stripe was read-locked")
	}
	s.For(7).RUnlock()
	s.RUnlock(7)
	if !s.For(7).TryLock() {
		t.Fatal("the stripe stayed locked after every reader left")
	}
	s.For(7).Unlock()
}

func TestLockKeysTakesEveryStripeOnceAndReleasesThemAll(t *testing.T) {
	s := NewStriped(4)
	var keys []uint64
	stripes := map[int]bool{}
	for k := uint64(0); len(stripes) < 2 || len(keys) < 6; k++ {
		if i := s.Index(k); len(stripes) < 2 || stripes[i] {
			stripes[i] = true
			keys = append(keys, k)
		}
	}
	done := make(chan func())
	go func() { done <- s.LockKeys(keys...) }()
	var unlock func()
	select {
	case unlock = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("LockKeys with two keys on one stripe hung: it locked a stripe twice")
	}
	for i := 0; i < s.Stripes(); i++ {
		if got := !s.At(i).TryLock(); got != stripes[i] {
			t.Fatalf("stripe %d locked = %v, want %v", i, got, stripes[i])
		} else if !got {
			s.At(i).Unlock()
		}
	}
	unlock()
	for i := 0; i < s.Stripes(); i++ {
		if !s.At(i).TryLock() {
			t.Fatalf("stripe %d is still locked after unlock", i)
		}
		s.At(i).Unlock()
	}
}

type logLock struct {
	id  int
	log *[]string
	mu  sync.Mutex
}

func (l *logLock) Lock() {
	if !l.mu.TryLock() {
		*l.log = append(*l.log, fmt.Sprintf("lock %d again", l.id))
		return
	}
	*l.log = append(*l.log, fmt.Sprintf("lock %d", l.id))
}

func (l *logLock) Unlock() {
	*l.log = append(*l.log, fmt.Sprintf("unlock %d", l.id))
	l.mu.TryLock()
	l.mu.Unlock()
}

func TestLockInOrderTakesLocksByAscendingIDOnceEachAndReleasesInReverse(t *testing.T) {
	var log []string
	ls := map[int]*logLock{}
	for _, id := range []int{1, 3, 5, 8} {
		ls[id] = &logLock{id: id, log: &log}
	}
	unlock, err := LockInOrder(
		Ordered{ID: 5, Lock: ls[5]}, Ordered{ID: 1, Lock: ls[1]}, Ordered{ID: 8, Lock: ls[8]},
		Ordered{ID: 3, Lock: ls[3]}, Ordered{ID: 1, Lock: ls[1]},
	)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	want := "[lock 1 lock 3 lock 5 lock 8 unlock 8 unlock 5 unlock 3 unlock 1]"
	if got := fmt.Sprint(log); got != want {
		t.Fatalf("LockInOrder did %s, want %s", got, want)
	}
	if unlock, err := LockInOrder(); err != nil {
		t.Fatal(err)
	} else {
		unlock()
	}
}

func TestLockInOrderRefusesANilLockBeforeTakingAnything(t *testing.T) {
	var log []string
	a := &logLock{id: 1, log: &log}
	if _, err := LockInOrder(Ordered{ID: 1, Lock: a}, Ordered{ID: 2}); !errors.Is(err, ErrNilLock) {
		t.Fatalf("LockInOrder with a nil lock gave %v", err)
	}
	if len(log) != 0 || !a.mu.TryLock() {
		t.Fatalf("a refused LockInOrder still took locks: %v", log)
	}
}

func TestTheChannelMutexLocksTriesAndCancels(t *testing.T) {
	var m Mutex
	m.Unlock()
	if !m.TryLock() {
		t.Fatal("a fresh mutex refused TryLock; an Unlock of an unlocked mutex must do nothing")
	}
	if m.TryLock() {
		t.Fatal("TryLock succeeded on a locked mutex")
	}
	cancel := make(chan struct{})
	close(cancel)
	if m.LockOrCancel(cancel) {
		t.Fatal("LockOrCancel took a locked mutex")
	}
	m.Unlock()
	if !m.LockOrCancel(make(chan struct{})) {
		t.Fatal("LockOrCancel did not take a free mutex")
	}
	m.Unlock()
	m.Lock()
	if m.TryLock() {
		t.Fatal("Lock did not lock")
	}
	m.Unlock()
}

type attempt struct {
	who    string
	second bool
}

func grabTwo(who string, first, second *Mutex, holding chan<- struct{}, proceed, cancel <-chan struct{}, out chan<- attempt) {
	first.Lock()
	holding <- struct{}{}
	<-proceed
	got := second.LockOrCancel(cancel)
	if got {
		second.Unlock()
	}
	first.Unlock()
	out <- attempt{who: who, second: got}
}

func TestTwoGoroutinesTakingTwoLocksInOppositeOrderDeadlockUntilReleased(t *testing.T) {
	var a, b Mutex
	holding := make(chan struct{}, 2)
	proceed := make(chan struct{})
	cancel := make(chan struct{})
	out := make(chan attempt, 2)
	go grabTwo("first", &a, &b, holding, proceed, cancel, out)
	go grabTwo("second", &b, &a, holding, proceed, cancel, out)
	<-holding
	<-holding
	close(proceed)
	select {
	case r := <-out:
		t.Fatalf("the %s goroutine finished (got its second lock: %v); two opposite-order lockers must deadlock", r.who, r.second)
	case <-time.After(time.Second):
	}
	close(cancel)
	for i := 0; i < 2; i++ {
		if r := <-out; r.second {
			t.Fatalf("the %s goroutine got its second lock; each held the lock the other wanted", r.who)
		}
	}
	if !a.TryLock() || !b.TryLock() {
		t.Fatal("releasing the deadlock left a lock held")
	}
}

type gateLock struct {
	m      *Mutex
	took   chan<- string
	name   string
	hold   <-chan struct{}
	paused bool
}

func (g *gateLock) Lock() {
	g.m.Lock()
	g.took <- g.name
	if g.hold != nil && !g.paused {
		g.paused = true
		<-g.hold
	}
}

func (g *gateLock) Unlock() { g.m.Unlock() }

func TestLockInOrderNeverDeadlocksGoroutinesThatListTheLocksInOppositeOrder(t *testing.T) {
	var a, b Mutex
	took := make(chan string, 8)
	hold := make(chan struct{})
	finished := make(chan string, 2)
	go func() {
		unlock, err := LockInOrder(Ordered{ID: 1, Lock: &gateLock{m: &a, took: took, name: "first:a", hold: hold}},
			Ordered{ID: 2, Lock: &gateLock{m: &b, took: took, name: "first:b"}})
		if err == nil {
			unlock()
		}
		finished <- "first"
	}()
	if got := <-took; got != "first:a" {
		t.Fatalf("the first goroutine took %s first", got)
	}
	go func() {
		unlock, err := LockInOrder(Ordered{ID: 2, Lock: &gateLock{m: &b, took: took, name: "second:b"}},
			Ordered{ID: 1, Lock: &gateLock{m: &a, took: took, name: "second:a"}})
		if err == nil {
			unlock()
		}
		finished <- "second"
	}()
	select {
	case got := <-took:
		t.Fatalf("while the first goroutine held a and waited, the second took %s: it went in the order it was given", got)
	case <-time.After(200 * time.Millisecond):
	}
	close(hold)
	var order []string
	for len(order) < 3 {
		select {
		case got := <-took:
			order = append(order, got)
		case <-time.After(30 * time.Second):
			t.Fatalf("deadlock: only %v happened", order)
		}
	}
	if fmt.Sprint(order) != "[first:b second:a second:b]" {
		t.Fatalf("locks were taken in the order %v", order)
	}
	for i := 0; i < 2; i++ {
		<-finished
	}
}

func TestManyGoroutinesUsingLockInOrderAllFinish(t *testing.T) {
	locks := make([]Mutex, 4)
	var total int
	var wg sync.WaitGroup
	rounds := 2000
	if testing.Short() {
		rounds = 300
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				x, y := (g+r)%4, (g+2*r+1)%4
				unlock, err := LockInOrder(Ordered{ID: uint64(y), Lock: &locks[y]}, Ordered{ID: uint64(x), Lock: &locks[x]},
					Ordered{ID: 0, Lock: &locks[0]})
				if err != nil {
					t.Error(err)
					return
				}
				total++
				unlock()
			}
		}(g)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("goroutines taking locks in sorted order deadlocked")
	}
	if total != 4*rounds {
		t.Fatalf("%d critical sections ran, want %d: lock 0 is in every set and must serialize them", total, 4*rounds)
	}
}
