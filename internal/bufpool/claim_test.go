package bufpool

import (
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func claimVictim(t *testing.T, p *Pool) (*Frame, core.PageID, uint64) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.rep.Evict()
	if !ok {
		t.Fatal("no victim")
	}
	f := p.frames[i]
	p.seq++
	f.claim = p.seq
	return f, f.id, p.seq
}

func TestAHitOnAPageAnEvictorHasPickedRescuesItAndTheEvictorPicksAgain(t *testing.T) {
	src, ids := seeded(t, 3)
	p := mustPool(t, src, Options{Frames: 2})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	f, id, token := claimVictim(t, p)
	if id != ids[0] {
		t.Fatalf("LRU picked page %d, want %d", id, ids[0])
	}
	g := mustFetch(t, p, id)
	if g != f {
		t.Fatal("the hit returned another frame")
	}
	won, err := p.evict(f, id, token)
	if won || err != nil || !p.Contains(id) || f.Pins() != 1 {
		t.Fatalf("the evictor took a frame a reader had just pinned: won %v err %v", won, err)
	}
	mustUnpin(t, p, g, false)
	h := mustFetch(t, p, ids[2])
	if p.Contains(ids[1]) || !p.Contains(id) {
		t.Fatal("after losing its claim the next eviction should take the other page")
	}
	mustUnpin(t, p, h, false)
}

func TestAFlushOfAPickedPageRescuesItToo(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 1
	mustUnpin(t, p, f, true)
	_, id, token := claimVictim(t, p)
	must(t, p.Flush(id))
	if won, err := p.evict(f, id, token); won || err != nil {
		t.Fatalf("the evictor kept a claim a flush had taken over: %v %v", won, err)
	}
	g := mustFetch(t, p, ids[1])
	if p.Contains(ids[0]) {
		t.Fatal("the rescued page was never evictable again")
	}
	mustUnpin(t, p, g, false)
}

func TestAFailedFreeOfAPickedPageLeavesItEvictable(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 1})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	f, id, token := claimVictim(t, p)
	fs.failFree = true
	if err := p.Free(id); !errors.Is(err, errInjected) {
		t.Fatalf("Free gave %v", err)
	}
	if won, _ := p.evict(f, id, token); won {
		t.Fatal("the evictor kept a claim that Free had taken over")
	}
	fs.failFree = false
	g := mustFetch(t, p, ids[1])
	if p.Contains(ids[0]) {
		t.Fatal("after a failed Free the page was never evictable again")
	}
	mustUnpin(t, p, g, false)
}

func TestASuccessfulFreeOfAPickedPageMakesTheEvictorPickAgain(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 1})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	f, id, token := claimVictim(t, p)
	must(t, p.Free(id))
	if won, _ := p.evict(f, id, token); won {
		t.Fatal("the evictor took a frame that Free had already given back")
	}
	g := mustFetch(t, p, ids[1])
	mustUnpin(t, p, g, false)
	if p.Resident() != 1 || p.Frames() != 1 {
		t.Fatal("the freed frame was lost")
	}
}

func TestNewRejectsAStripeCountOutOfRangeAndRoundsTheRestUp(t *testing.T) {
	for _, n := range []int{-1, MaxStripes + 1} {
		if _, err := New(NewMemSource(), Options{Frames: 1, Stripes: n}); !errors.Is(err, ErrBadOptions) {
			t.Fatalf("New with %d stripes gave %v", n, err)
		}
	}
	for n, want := range map[int]int{0: DefaultStripes, 1: 1, 5: 8, MaxStripes: MaxStripes} {
		p, err := New(NewMemSource(), Options{Frames: 1, Stripes: n})
		if err != nil || p.Options().Stripes != want {
			t.Fatalf("New with %d stripes gave %v and %d stripes, want %d", n, err, p.Options().Stripes, want)
		}
	}
}

func TestNewPageDroppingAPickedStaleFrameMakesTheEvictorPickAgain(t *testing.T) {
	src := NewMemSource()
	p := mustPool(t, src, Options{Frames: 2})
	f, err := p.NewPage(page.KindHeap)
	must(t, err)
	id := f.ID()
	mustUnpin(t, p, f, false)
	picked, pid, token := claimVictim(t, p)
	if picked != f || pid != id {
		t.Fatalf("picked page %d, want %d", pid, id)
	}
	must(t, src.Free(id))
	g, err := p.NewPage(page.KindHeap)
	must(t, err)
	if g.ID() != id || g == f {
		t.Fatalf("NewPage gave page %d in the stale frame %v", g.ID(), g == f)
	}
	if won, _ := p.evict(f, id, token); won {
		t.Fatal("the evictor took a stale frame NewPage had already dropped")
	}
	if !p.Contains(id) || g.Pins() != 1 {
		t.Fatal("the evictor's stale claim removed the new page from the page table")
	}
	mustUnpin(t, p, g, false)
}
