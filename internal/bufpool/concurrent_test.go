package bufpool

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

const generous = 30 * time.Second

type gate struct {
	entered chan core.PageID
	open    chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan core.PageID, 16), open: make(chan struct{})} }

func (g *gate) pass(id core.PageID) {
	g.entered <- id
	<-g.open
}

type gatedSource struct {
	Source
	mu        sync.Mutex
	readGate  map[core.PageID]*gate
	writeGate map[core.PageID]*gate
	failRead  map[core.PageID]int
	reads     map[core.PageID]int
	writes    map[core.PageID]int
}

func newGatedSource(inner Source) *gatedSource {
	return &gatedSource{Source: inner, readGate: map[core.PageID]*gate{}, writeGate: map[core.PageID]*gate{},
		failRead: map[core.PageID]int{}, reads: map[core.PageID]int{}, writes: map[core.PageID]int{}}
}

func (s *gatedSource) Read(id core.PageID) (page.Page, error) {
	s.mu.Lock()
	g := s.readGate[id]
	s.reads[id]++
	fail := s.failRead[id] > 0
	if fail {
		s.failRead[id]--
	}
	s.mu.Unlock()
	if g != nil {
		g.pass(id)
	}
	if fail {
		return nil, errInjected
	}
	return s.Source.Read(id)
}

func (s *gatedSource) Write(p page.Page) error {
	s.mu.Lock()
	g := s.writeGate[p.ID()]
	s.writes[p.ID()]++
	s.mu.Unlock()
	if g != nil {
		g.pass(p.ID())
	}
	return s.Source.Write(p)
}

func (s *gatedSource) count(m map[core.PageID]int, id core.PageID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m[id]
}

func waitFor(t *testing.T, ch <-chan core.PageID, what string) core.PageID {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(generous):
		t.Fatalf("gave up waiting for %s", what)
		return 0
	}
}

type fetched struct {
	f   *Frame
	err error
}

func fetchAsync(p *Pool, id core.PageID) <-chan fetched {
	out := make(chan fetched, 1)
	go func() {
		f, err := p.Fetch(id)
		out <- fetched{f, err}
	}()
	return out
}

func waiting(p *Pool) <-chan core.PageID {
	ch := make(chan core.PageID, 16)
	p.onWait = func(id core.PageID) { ch <- id }
	return ch
}

func TestConcurrentFetchesOfOnePageWaitForTheOneLoad(t *testing.T) {
	src, ids := seeded(t, 1)
	gs := newGatedSource(src)
	g := newGate()
	gs.readGate[ids[0]] = g
	p := mustPool(t, gs, Options{Frames: 2})
	waits := waiting(p)
	first := fetchAsync(p, ids[0])
	waitFor(t, g.entered, "the first read")
	second := fetchAsync(p, ids[0])
	if id := waitFor(t, waits, "the second Fetch to wait"); id != ids[0] {
		t.Fatalf("the second Fetch waited on page %d", id)
	}
	close(g.open)
	a, b := <-first, <-second
	if a.err != nil || b.err != nil || a.f != b.f {
		t.Fatalf("fetches gave %v and %v, frames equal %v", a.err, b.err, a.f == b.f)
	}
	if a.f.Pins() != 2 || a.f.Page()[page.HeaderSize] != 0 || gs.count(gs.reads, ids[0]) != 1 {
		t.Fatalf("pins %d, reads %d: both fetches must share one fully loaded frame", a.f.Pins(), gs.count(gs.reads, ids[0]))
	}
	if s := p.Stats(); s.Misses != 1 || s.Hits != 1 || s.Reads != 1 {
		t.Fatalf("stats %+v, want one miss and one hit", s)
	}
	mustUnpin(t, p, a.f, false)
	mustUnpin(t, p, b.f, false)
}

func TestAFailedLoadWakesItsWaitersAndOneOfThemLoadsThePageAgain(t *testing.T) {
	src, ids := seeded(t, 1)
	gs := newGatedSource(src)
	g := newGate()
	gs.readGate[ids[0]] = g
	gs.failRead[ids[0]] = 1
	p := mustPool(t, gs, Options{Frames: 1})
	waits := waiting(p)
	first := fetchAsync(p, ids[0])
	waitFor(t, g.entered, "the first read")
	second := fetchAsync(p, ids[0])
	waitFor(t, waits, "the second Fetch to wait")
	gs.mu.Lock()
	delete(gs.readGate, ids[0])
	gs.mu.Unlock()
	close(g.open)
	if r := <-first; !errors.Is(r.err, errInjected) {
		t.Fatalf("the failed load gave %v", r.err)
	}
	r := <-second
	if r.err != nil || r.f.Page()[page.HeaderSize] != 0 || r.f.ID() != ids[0] {
		t.Fatalf("the waiter gave %v after the load it waited for failed", r.err)
	}
	if gs.count(gs.reads, ids[0]) != 2 || p.Pinned() != 1 {
		t.Fatalf("reads %d, pinned %d", gs.count(gs.reads, ids[0]), p.Pinned())
	}
	mustUnpin(t, p, r.f, false)
}

func TestAFetchOfADirtyPageBeingEvictedWaitsForTheWriteAndReadsTheNewBytes(t *testing.T) {
	src, ids := seeded(t, 4)
	a, b, c, d := ids[0], ids[1], ids[2], ids[3]
	gs := newGatedSource(src)
	p := mustPool(t, gs, Options{Frames: 3})
	waits := waiting(p)
	f := mustFetch(t, p, a)
	f.Page()[page.HeaderSize] = 0xAB
	mustUnpin(t, p, f, true)
	hc, hd := mustFetch(t, p, c), mustFetch(t, p, d)
	g := newGate()
	gs.writeGate[a] = g
	evictor := fetchAsync(p, b)
	waitFor(t, g.entered, "the eviction write of the dirty page")
	if !p.Contains(a) {
		t.Fatal("the dirty victim left the page table before its write finished")
	}
	reader := fetchAsync(p, a)
	if id := waitFor(t, waits, "the Fetch of the page in transit to wait"); id != a {
		t.Fatalf("the reader waited on page %d", id)
	}
	mustUnpin(t, p, hd, false)
	if err := p.Free(d); err != nil {
		t.Fatal(err)
	}
	close(g.open)
	e := <-evictor
	r := <-reader
	if e.err != nil || r.err != nil {
		t.Fatalf("evictor %v, reader %v", e.err, r.err)
	}
	if r.f.Page()[page.HeaderSize] != 0xAB || r.f.Dirty() {
		t.Fatalf("the reader saw byte %02X, want the evicted change AB", r.f.Page()[page.HeaderSize])
	}
	if gs.count(gs.writes, a) != 1 || gs.count(gs.reads, a) != 2 {
		t.Fatalf("page %d written %d times and read %d times", a, gs.count(gs.writes, a), gs.count(gs.reads, a))
	}
	mustUnpin(t, p, e.f, false)
	mustUnpin(t, p, r.f, false)
	mustUnpin(t, p, hc, false)
}

func TestFetchDoesNotWaitForAFrameThatIsBeingEvicted(t *testing.T) {
	src, ids := seeded(t, 3)
	gs := newGatedSource(src)
	p := mustPool(t, gs, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 1
	mustUnpin(t, p, f, true)
	g := newGate()
	gs.writeGate[ids[0]] = g
	evictor := fetchAsync(p, ids[1])
	waitFor(t, g.entered, "the eviction write")
	if _, err := p.Fetch(ids[2]); !errors.Is(err, ErrNoFreeFrame) {
		t.Fatalf("Fetch while the only frame is in transit gave %v, want ErrNoFreeFrame at once", err)
	}
	close(g.open)
	e := <-evictor
	if e.err != nil {
		t.Fatal(e.err)
	}
	mustUnpin(t, p, e.f, false)
}

func TestFlushHoldsTheReadLatchUntilItsWriteIsDone(t *testing.T) {
	src, ids := seeded(t, 1)
	gs := newGatedSource(src)
	p := mustPool(t, gs, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x11
	mustUnpin(t, p, f, true)
	g := newGate()
	gs.writeGate[ids[0]] = g
	flushed := make(chan error, 1)
	go func() { flushed <- p.Flush(ids[0]) }()
	waitFor(t, g.entered, "the flush write")
	if f.latch.TryLock() {
		t.Fatal("a writer could latch the page while a flush was writing it")
	}
	if !f.latch.TryRLock() {
		t.Fatal("a reader was kept out while a flush was writing the page")
	}
	f.latch.RUnlock()
	if !f.Dirty() {
		t.Fatal("the page was marked clean before its write finished")
	}
	close(g.open)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if f.Dirty() || !latchFree(f) || sourceByte(t, src, ids[0]) != 0x11 {
		t.Fatal("after the flush the page is still dirty, latched, or unwritten")
	}
}

func loadingBehindAGate(t *testing.T) (*Pool, *gatedSource, *gate, core.PageID, <-chan fetched, <-chan core.PageID) {
	t.Helper()
	src, ids := seeded(t, 1)
	gs := newGatedSource(src)
	g := newGate()
	gs.readGate[ids[0]] = g
	p := mustPool(t, gs, Options{Frames: 1})
	waits := waiting(p)
	loader := fetchAsync(p, ids[0])
	waitFor(t, g.entered, "the read")
	return p, gs, g, ids[0], loader, waits
}

func TestFlushAndFreeWaitForAPageThatIsStillLoading(t *testing.T) {
	p, _, g, id, loader, waits := loadingBehindAGate(t)
	flushed := make(chan error, 1)
	go func() { flushed <- p.Flush(id) }()
	waitFor(t, waits, "Flush to wait for the load")
	close(g.open)
	l := <-loader
	if l.err != nil {
		t.Fatal(l.err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("Flush of a page that finished loading clean gave %v", err)
	}
	mustUnpin(t, p, l.f, false)

	p, gs, g, id, loader, waits := loadingBehindAGate(t)
	freed := make(chan error, 1)
	go func() { freed <- p.Free(id) }()
	waitFor(t, waits, "Free to wait for the load")
	close(g.open)
	l = <-loader
	if l.err != nil {
		t.Fatal(l.err)
	}
	if err := <-freed; !errors.Is(err, ErrPinned) {
		t.Fatalf("Free of a page that finished loading pinned gave %v, want ErrPinned", err)
	}
	mustUnpin(t, p, l.f, false)
	must(t, p.Free(id))
	if p.Contains(id) || gs.count(gs.reads, id) != 1 {
		t.Fatal("Free after the load did not drop the page")
	}
}

func TestNewPageRefusesAnIDWhoseOldFrameIsStillLoading(t *testing.T) {
	src := NewMemSource()
	first, err := src.Allocate(page.KindHeap)
	must(t, err)
	gs := newGatedSource(src)
	g := newGate()
	gs.readGate[first.ID()] = g
	p := mustPool(t, gs, Options{Frames: 2})
	loader := fetchAsync(p, first.ID())
	waitFor(t, g.entered, "the read")
	must(t, src.Free(first.ID()))
	if _, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrPinned) {
		t.Fatalf("NewPage of an id whose frame is loading gave %v, want ErrPinned", err)
	}
	if p.Pinned() != 1 {
		t.Fatalf("%d frames pinned after the refusal; only the loader's pin should remain", p.Pinned())
	}
	close(g.open)
	l := <-loader
	if l.err != nil {
		t.Fatal(l.err)
	}
	mustUnpin(t, p, l.f, false)
	if p.Pinned() != 0 {
		t.Fatalf("pinned %d", p.Pinned())
	}
}

func patternByte(id core.PageID, counter uint64, i int) byte {
	return byte(uint64(id)*31 + counter*7 + uint64(i))
}

const (
	offCounter = page.HeaderSize
	offBalance = page.HeaderSize + 8
	offPattern = page.HeaderSize + 16
)

func stamp(p page.Page, counter uint64) {
	binary.BigEndian.PutUint64(p[offCounter:], counter)
	for i := offPattern; i < page.Size; i++ {
		p[i] = patternByte(p.ID(), counter, i)
	}
}

func torn(p page.Page) error {
	counter := binary.BigEndian.Uint64(p[offCounter:])
	for i := offPattern; i < page.Size; i++ {
		if p[i] != patternByte(p.ID(), counter, i) {
			return fmt.Errorf("page %d is torn: counter %d, byte %d is %02X, want %02X", p.ID(), counter, i, p[i], patternByte(p.ID(), counter, i))
		}
	}
	return nil
}

func balance(p page.Page) int64 { return int64(binary.BigEndian.Uint64(p[offBalance:])) }

func setBalance(p page.Page, v int64) { binary.BigEndian.PutUint64(p[offBalance:], uint64(v)) }

type stressConfig struct {
	pol     Policy
	mode    Mode
	stripes int
}

func TestManyGoroutinesSharingThePoolNeverTearOrLoseAnUpdate(t *testing.T) {
	const goroutines, start = 8, 1000
	frames := goroutines * (1 + 3)
	pages := 3 * frames
	ops := 3000
	if testing.Short() {
		ops = 600
	}
	for _, c := range []stressConfig{{LRU, WriteBack, 16}, {Clock, WriteBack, 1}, {LRUK, WriteBack, 64}, {LRU, WriteThrough, 4}} {
		t.Run(fmt.Sprintf("%s-%s-%dstripes", c.pol, c.mode, c.stripes), func(t *testing.T) {
			src := NewMemSource()
			var ids []core.PageID
			for i := 0; i < pages; i++ {
				p, err := src.Allocate(page.KindHeap)
				must(t, err)
				stamp(p, 0)
				setBalance(p, start)
				must(t, src.Write(p))
				ids = append(ids, p.ID())
			}
			p := mustPool(t, src, Options{Frames: frames, Policy: c.pol, Mode: c.mode, Stripes: c.stripes})
			incs := make([]atomic.Uint64, pages)
			var aborted, committed atomic.Uint64
			var wg sync.WaitGroup
			for g := 0; g < goroutines; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					r := rand.New(rand.NewSource(int64(g) + 1))
					for n := 0; n < ops; n++ {
						if err := stressStep(p, r, ids, incs, &aborted, &committed); err != nil {
							t.Error(err)
							return
						}
					}
				}(g)
			}
			wg.Wait()
			if t.Failed() {
				return
			}
			if p.Pinned() != 0 || p.Resident() > p.Frames() {
				t.Fatalf("after the run %d frames are pinned and %d pages resident in %d frames", p.Pinned(), p.Resident(), p.Frames())
			}
			must(t, p.FlushAll())
			var total int64
			for i, id := range ids {
				pg, err := src.Read(id)
				must(t, err)
				if err := torn(pg); err != nil {
					t.Fatal(err)
				}
				if got, want := binary.BigEndian.Uint64(pg[offCounter:]), incs[i].Load(); got != want {
					t.Fatalf("page %d counter is %d after %d increments: an update was lost", id, got, want)
				}
				total += balance(pg)
			}
			if total != int64(pages*start) {
				t.Fatalf("balances sum to %d, want %d: a transfer was half applied", total, pages*start)
			}
			if committed.Load() == 0 || aborted.Load() == 0 || p.Stats().Evictions == 0 {
				t.Fatalf("committed %d, aborted %d, evictions %d: the run did not exercise everything",
					committed.Load(), aborted.Load(), p.Stats().Evictions)
			}
		})
	}
}

func stressStep(p *Pool, r *rand.Rand, ids []core.PageID, incs []atomic.Uint64, aborted, committed *atomic.Uint64) error {
	i := r.Intn(len(ids))
	id := ids[i]
	switch op := r.Intn(10); {
	case op < 4:
		f, err := p.Fetch(id)
		if err != nil {
			return err
		}
		p.WLatch(f)
		err = torn(f.Page())
		stamp(f.Page(), binary.BigEndian.Uint64(f.Page()[offCounter:])+1)
		incs[i].Add(1)
		p.markDirty(f)
		p.WUnlatch(f)
		if uerr := p.Unpin(f, true); err == nil {
			err = uerr
		}
		return err
	case op < 7:
		f, err := p.Fetch(id)
		if err != nil {
			return err
		}
		p.RLatch(f)
		err = torn(f.Page())
		p.RUnlatch(f)
		if uerr := p.Unpin(f, false); err == nil {
			err = uerr
		}
		return err
	case op < 9:
		j := r.Intn(len(ids))
		if j == i {
			return nil
		}
		if j < i {
			i, j = j, i
		}
		m := p.Begin()
		var frames []*Frame
		for _, k := range []int{i, j} {
			f, err := m.Fetch(ids[k])
			if err != nil {
				return errors.Join(err, m.Abort())
			}
			if err := m.WLatch(f); err != nil {
				return errors.Join(err, m.Abort())
			}
			if err := torn(f.Page()); err != nil {
				return errors.Join(err, m.Abort())
			}
			stamp(f.Page(), binary.BigEndian.Uint64(f.Page()[offCounter:])+1)
			frames = append(frames, f)
		}
		setBalance(frames[0].Page(), balance(frames[0].Page())-1)
		setBalance(frames[1].Page(), balance(frames[1].Page())+1)
		if r.Intn(4) == 0 {
			aborted.Add(1)
			return m.Abort()
		}
		incs[i].Add(1)
		incs[j].Add(1)
		committed.Add(1)
		return m.Commit()
	default:
		if r.Intn(20) == 0 {
			return p.FlushAll()
		}
		return p.Flush(id)
	}
}
