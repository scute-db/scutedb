package bufpool

import (
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/page"
)

func TestClockFindsTheOnlyEvictableFrameEvenWhenItSitsJustBehindTheHand(t *testing.T) {
	for n := 1; n <= 6; n++ {
		r := NewClock(n)
		for f := 0; f < n; f++ {
			r.Access(f)
		}
		r.SetEvictable(n-1, true)
		if f, ok := r.Evict(); !ok || f != n-1 {
			t.Fatalf("%d frames: Evict gave %d %v, want the only evictable frame %d after clearing its bit and coming round again", n, f, ok, n-1)
		}
	}
}

func TestAFrameFreedWhileDirtyIsCleanWhenItHoldsTheNextPage(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x66
	mustUnpin(t, p, f, true)
	must(t, p.Free(ids[0]))
	g := mustFetch(t, p, ids[1])
	if g.Dirty() {
		t.Fatal("the next page inherited the freed page's dirty flag")
	}
	mustUnpin(t, p, g, false)
	must(t, p.FlushAll())
	if len(fs.writes) != 0 {
		t.Fatalf("writes %v; nothing was changed after the free", fs.writes)
	}
}

func TestAFrameTakenForANewPageThatFailsIsNotCountedAsResident(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		src, ids := seeded(t, 1)
		fs := newFaultSource(src)
		p := mustPool(t, fs, Options{Frames: 1})
		f := mustFetch(t, p, ids[0])
		f.Page()[page.HeaderSize] = 0x21
		mustUnpin(t, p, f, dirty)
		fs.failAlloc = true
		if _, err := p.NewPage(page.KindHeap); !errors.Is(err, errInjected) {
			t.Fatalf("NewPage gave %v", err)
		}
		if p.Resident() != 0 || p.Contains(ids[0]) || p.Pinned() != 0 {
			t.Fatalf("dirty %v: after the evicted frame went back unused, %d pages are resident", dirty, p.Resident())
		}
		if dirty && sourceByte(t, src, ids[0]) != 0x21 {
			t.Fatal("the dirty victim was not written")
		}
		fs.failAlloc = false
		g, err := p.NewPage(page.KindHeap)
		must(t, err)
		if p.Resident() != 1 {
			t.Fatalf("%d pages resident after one NewPage", p.Resident())
		}
		mustUnpin(t, p, g, false)
	}
}

func TestUnpinOfAStaleFrameNowLoadingAnotherPageIsRefused(t *testing.T) {
	src, ids := seeded(t, 2)
	gs := newGatedSource(src)
	p := mustPool(t, gs, Options{Frames: 1})
	stale := mustFetch(t, p, ids[0])
	mustUnpin(t, p, stale, false)
	g := newGate()
	gs.readGate[ids[1]] = g
	loader := fetchAsync(p, ids[1])
	waitFor(t, g.entered, "the read of the next page")
	if err := p.Unpin(stale, false); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("Unpin of a frame that is loading another page gave %v, want ErrNotPinned", err)
	}
	close(g.open)
	l := <-loader
	if l.err != nil || l.f != stale || l.f.Pins() != 1 {
		t.Fatalf("the loader got %v, same frame %v, pins %d", l.err, l.f == stale, l.f.Pins())
	}
	mustUnpin(t, p, l.f, false)
}
