package bufpool

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func TestResidentCountsAFrameThatIsStillLoading(t *testing.T) {
	p, _, g, _, loader, _ := loadingBehindAGate(t)
	if n := p.Resident(); n != 1 {
		t.Fatalf("Resident() = %d while the only frame is loading a page, want 1", n)
	}
	close(g.open)
	l := <-loader
	must(t, l.err)
	mustUnpin(t, p, l.f, false)
}

func TestPinsDirtyAndContainsAreSafeWhileOtherGoroutinesFetch(t *testing.T) {
	src, ids := seeded(t, 8)
	p := mustPool(t, src, Options{Frames: 3, Stripes: 1})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3000; i++ {
			g, err := p.Fetch(ids[i%len(ids)])
			if err != nil {
				t.Error(err)
				return
			}
			if err := p.Unpin(g, i%3 == 0); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		for _, f := range p.frames {
			_ = f.Pins()
			_ = f.Dirty()
		}
		for _, id := range ids {
			_ = p.Contains(id)
		}
	}
}

func TestAFailedLoadLeavesNoLRUKHistoryForTheNextPageInThatFrame(t *testing.T) {
	src, ids := seeded(t, 4)
	a, b, c, z := ids[0], ids[1], ids[2], ids[3]
	fs := newFaultSource(src)
	fs.failRead[z] = true
	p := mustPool(t, fs, Options{Frames: 2, Policy: LRUK, K: 2})
	if _, err := p.Fetch(z); !errors.Is(err, errInjected) {
		t.Fatalf("Fetch of the failing page gave %v", err)
	}
	mustUnpin(t, p, mustFetch(t, p, a), false)
	mustUnpin(t, p, mustFetch(t, p, b), false)
	mustUnpin(t, p, mustFetch(t, p, c), false)
	if p.Contains(a) || !p.Contains(b) {
		t.Fatalf("LRU-K kept page %d (resident %v) over page %d (resident %v): the failed load's access was inherited", a, p.Contains(a), b, p.Contains(b))
	}
}

func TestNewPageRefusesAnIDWhoseOldFrameIsStillBeingWrittenOut(t *testing.T) {
	src, ids := seeded(t, 3)
	gs := newGatedSource(src)
	p := mustPool(t, gs, Options{Frames: 2})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0xEE
	mustUnpin(t, p, f, true)
	held := mustFetch(t, p, ids[1])
	g := newGate()
	gs.writeGate[ids[0]] = g
	evictor := fetchAsync(p, ids[2])
	waitFor(t, g.entered, "the eviction write")
	mustUnpin(t, p, held, false)
	must(t, src.Free(ids[0]))
	if nf, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrPinned) {
		t.Fatalf("NewPage of an id whose old frame is still being written out gave %v (page %v)", err, nf != nil && nf.ID() == ids[0])
	}
	close(g.open)
	e := <-evictor
	must(t, e.err)
	mustUnpin(t, p, e.f, false)
	if p.Pinned() != 0 {
		t.Fatalf("pinned %d", p.Pinned())
	}
}

func TestAFlushAfterAFailedEvictionIsNotAnAccess(t *testing.T) {
	src, ids := seeded(t, 3)
	a, b, c := ids[0], ids[1], ids[2]
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	f := mustFetch(t, p, a)
	f.Page()[page.HeaderSize] = 0x5A
	mustUnpin(t, p, f, true)
	mustUnpin(t, p, mustFetch(t, p, b), false)
	fs.failWrite[a] = true
	if _, err := p.Fetch(c); !errors.Is(err, errInjected) {
		t.Fatalf("Fetch that had to evict the unwritable page gave %v", err)
	}
	mustUnpin(t, p, mustFetch(t, p, b), false)
	fs.failWrite[a] = false
	must(t, p.Flush(a))
	mustUnpin(t, p, mustFetch(t, p, c), false)
	if p.Contains(a) || !p.Contains(b) {
		t.Fatalf("after Flush(%d) the next eviction took page %d instead: the flush counted as an access", a, b)
	}
}

func TestAPageWhoseEvictionWriteFailedCanStillBeFetched(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0xD2
	mustUnpin(t, p, f, true)
	fs.failWrite[ids[0]] = true
	if _, err := p.Fetch(ids[1]); !errors.Is(err, errInjected) {
		t.Fatalf("Fetch that had to evict an unwritable page gave %v", err)
	}
	select {
	case r := <-fetchAsync(p, ids[0]):
		if r.err != nil || r.f != f || r.f.Page()[page.HeaderSize] != 0xD2 {
			t.Fatalf("Fetch of the page whose eviction failed gave %v", r.err)
		}
		mustUnpin(t, p, r.f, false)
	case <-time.After(generous):
		t.Fatal("a Fetch of a page whose eviction write failed never returned")
	}
}

type stampingSource struct {
	Source
}

func (s stampingSource) Write(p page.Page) error {
	p[12] = 0xC5
	return s.Source.Write(p)
}

func TestTheSourceIsHandedAPrivateCopyNeverTheLiveFrame(t *testing.T) {
	src, ids := seeded(t, 1)
	p := mustPool(t, stampingSource{src}, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x10
	mustUnpin(t, p, f, true)
	must(t, p.Flush(ids[0]))
	if f.Page()[12] != 0 {
		t.Fatalf("the source's write changed the frame itself: header byte 12 is %02X", f.Page()[12])
	}
}

func TestASecondFlushThatFindsThePageAlreadyWrittenDoesNotWriteItAgain(t *testing.T) {
	src, ids := seeded(t, 1)
	gs := newGatedSource(src)
	p := mustPool(t, gs, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x3C
	mustUnpin(t, p, f, true)
	g := newGate()
	gs.writeGate[ids[0]] = g
	first := make(chan error, 1)
	go func() { first <- p.Flush(ids[0]) }()
	waitFor(t, g.entered, "the first flush write")
	writer := make(chan struct{})
	go func() {
		p.WLatch(f)
		p.WUnlatch(f)
		close(writer)
	}()
	for f.latch.TryRLock() {
		f.latch.RUnlock()
		runtime.Gosched()
	}
	second := make(chan error, 1)
	go func() { second <- p.Flush(ids[0]) }()
	for f.Pins() < 2 {
		runtime.Gosched()
	}
	close(g.open)
	must(t, <-first)
	<-writer
	must(t, <-second)
	if n := gs.count(gs.writes, ids[0]); n != 1 {
		t.Fatalf("page written %d times; the second flush found it clean and should not write", n)
	}
}

func TestAFlushOfACleanPickedPageLeavesTheEvictorItsClaim(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 1})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	f, id, token := claimVictim(t, p)
	must(t, p.Flush(id))
	if won, err := p.evict(f, id, token); !won || err != nil {
		t.Fatalf("a Flush of a clean page took the evictor's claim: won %v, err %v", won, err)
	}
	p.giveBack(f)
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
}

func TestFlushIsNotAReplacerAccess(t *testing.T) {
	src, ids := seeded(t, 3)
	p := mustPool(t, src, Options{Frames: 2})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 1
	mustUnpin(t, p, f, true)
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	must(t, p.Flush(ids[0]))
	mustUnpin(t, p, mustFetch(t, p, ids[2]), false)
	if p.Contains(ids[0]) || !p.Contains(ids[1]) {
		t.Fatal("Flush moved the flushed page up the LRU order")
	}
}

func TestFlushAllDoesNotWaitForACleanPageThatIsStillLoading(t *testing.T) {
	p, _, g, _, loader, waits := loadingBehindAGate(t)
	done := make(chan error, 1)
	go func() { done <- p.FlushAll() }()
	select {
	case err := <-done:
		must(t, err)
	case id := <-waits:
		t.Fatalf("FlushAll waited for page %d, which is clean and still loading", id)
	}
	close(g.open)
	l := <-loader
	must(t, l.err)
	mustUnpin(t, p, l.f, false)
}

func TestFlushAndFreeOfAPageBeingWrittenOutWaitForTheWrite(t *testing.T) {
	for _, op := range []string{"flush", "free"} {
		src, ids := seeded(t, 2)
		gs := newGatedSource(src)
		p := mustPool(t, gs, Options{Frames: 1})
		waits := waiting(p)
		f := mustFetch(t, p, ids[0])
		f.Page()[page.HeaderSize] = 0x5A
		mustUnpin(t, p, f, true)
		g := newGate()
		gs.writeGate[ids[0]] = g
		evictor := fetchAsync(p, ids[1])
		waitFor(t, g.entered, "the eviction write")
		res := make(chan error, 1)
		go func() {
			if op == "flush" {
				res <- p.Flush(ids[0])
			} else {
				res <- p.Free(ids[0])
			}
		}()
		select {
		case id := <-waits:
			if id != ids[0] {
				t.Fatalf("%s waited on page %d", op, id)
			}
		case <-g.entered:
			t.Fatalf("%s wrote page %d again while its eviction write was still running", op, ids[0])
		case err := <-res:
			t.Fatalf("%s returned %v before the eviction write finished", op, err)
		case <-time.After(generous):
			t.Fatalf("%s neither waited nor returned", op)
		}
		close(g.open)
		e := <-evictor
		if e.err != nil {
			t.Fatalf("%s: the evictor gave %v", op, e.err)
		}
		if err := <-res; err != nil {
			t.Fatalf("%s after the eviction gave %v", op, err)
		}
		if gs.count(gs.writes, ids[0]) != 1 || op == "flush" && sourceByte(t, src, ids[0]) != 0x5A {
			t.Fatalf("%s: page %d written %d times", op, ids[0], gs.count(gs.writes, ids[0]))
		}
		if op == "free" && src.Len() != 1 {
			t.Fatalf("free: the source still holds %d pages", src.Len())
		}
		mustUnpin(t, p, e.f, false)
	}
}

func TestAFailedFreeOfAPageNoEvictorPickedIsNotAnAccess(t *testing.T) {
	src, ids := seeded(t, 3)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	fs.failFree = true
	if err := p.Free(ids[0]); !errors.Is(err, errInjected) {
		t.Fatalf("Free gave %v", err)
	}
	fs.failFree = false
	mustUnpin(t, p, mustFetch(t, p, ids[2]), false)
	if p.Contains(ids[0]) || !p.Contains(ids[1]) {
		t.Fatal("a failed Free moved the page up the LRU order")
	}
}

type freeGatedSource struct {
	Source
	g *gate
}

func (s freeGatedSource) Free(id core.PageID) error {
	s.g.pass(id)
	return s.Source.Free(id)
}

func TestAFetchCannotPinAPageWhileFreeIsGivingItBackToTheSource(t *testing.T) {
	src, ids := seeded(t, 1)
	g := newGate()
	p := mustPool(t, freeGatedSource{src, g}, Options{Frames: 1, Stripes: 1})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	freed := make(chan error, 1)
	go func() { freed <- p.Free(ids[0]) }()
	waitFor(t, g.entered, "the source Free")
	mu, _ := p.stripe(ids[0])
	if mu.TryRLock() {
		mu.RUnlock()
		close(g.open)
		t.Fatal("the page table stripe was open while Free was freeing the page in the source")
	}
	fetch := fetchAsync(p, ids[0])
	close(g.open)
	must(t, <-freed)
	if r := <-fetch; !errors.Is(r.err, ErrNoSuchPage) || p.Pinned() != 0 {
		t.Fatalf("a Fetch racing with Free gave %v and left %d pins; the page is gone", r.err, p.Pinned())
	}
}

func TestAnAbortGivesItsNewPagesBackNewestFirst(t *testing.T) {
	p := mustPool(t, NewMemSource(), Options{Frames: 4})
	var first []core.PageID
	m := p.Begin()
	for i := 0; i < 3; i++ {
		f, err := m.NewPage(page.KindHeap)
		must(t, err)
		first = append(first, f.ID())
	}
	must(t, m.Abort())
	m = p.Begin()
	for i := 0; i < 3; i++ {
		f, err := m.NewPage(page.KindHeap)
		must(t, err)
		if f.ID() != first[i] {
			t.Fatalf("retry allocation %d got page %d, the aborted MTR had %d", i, f.ID(), first[i])
		}
	}
	must(t, m.Commit())
}

func TestAnAbortKeepsTheVerifiedMarkOfPagesItOnlyReadOrPinned(t *testing.T) {
	src, ids := seeded(t, 3)
	p := mustPool(t, src, Options{Frames: 3})
	m := p.Begin()
	a, b, c := mtrFetch(t, m, ids[0]), mtrFetch(t, m, ids[1]), mtrFetch(t, m, ids[2])
	a.MarkVerified()
	b.MarkVerified()
	must(t, m.RLatch(a))
	must(t, m.WLatch(c))
	c.Page()[page.HeaderSize] = 9
	must(t, m.Abort())
	if !a.Verified() || !b.Verified() {
		t.Fatalf("abort cleared the verified mark of a read-latched (%v) or pinned-only (%v) page", a.Verified(), b.Verified())
	}
}

func TestReleaseDropsItsLatchBeforeTheUnpinWritesThePageThrough(t *testing.T) {
	src, ids := seeded(t, 1)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 1, Mode: WriteThrough})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x61
	fs.failWrite[ids[0]] = true
	if err := p.Unpin(f, true); !errors.Is(err, errInjected) {
		t.Fatalf("the failing write-through gave %v", err)
	}
	delete(fs.failWrite, ids[0])
	m := p.Begin()
	g := mtrFetch(t, m, ids[0])
	must(t, m.WLatch(g))
	released := make(chan error, 1)
	go func() { released <- m.Release(g) }()
	select {
	case err := <-released:
		must(t, err)
	case <-time.After(generous):
		t.Fatal("Release deadlocked: its unpin wrote the page through while it still held the write latch")
	}
	if g.Dirty() || sourceByte(t, src, ids[0]) != 0x61 {
		t.Fatal("the released page was not written through")
	}
	must(t, m.Commit())
}

func TestALoadWakesItsWaitersOnlyAfterItHasPublishedThePage(t *testing.T) {
	p, _, g, id, loader, _ := loadingBehindAGate(t)
	f, ok := p.peek(id)
	if !ok {
		t.Fatal("the loading page is not in the page table")
	}
	p.mu.Lock()
	done := f.done
	p.mu.Unlock()
	mu, _ := p.stripe(id)
	mu.RLock()
	close(g.open)
	for mu.TryRLock() {
		mu.RUnlock()
		runtime.Gosched()
	}
	select {
	case <-done:
		mu.RUnlock()
		t.Fatal("the load woke its waiters before it took the locks that publish the page")
	default:
	}
	mu.RUnlock()
	l := <-loader
	must(t, l.err)
	select {
	case <-done:
	default:
		t.Fatal("the finished load never woke its waiters")
	}
	mustUnpin(t, p, l.f, false)
}
