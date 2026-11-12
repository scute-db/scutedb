package bufpool

import (
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func TestClockThroughThePoolEvictsPagesInTheOrderTheyWereLoaded(t *testing.T) {
	src, ids := seeded(t, 4)
	p := mustPool(t, src, Options{Frames: 3, Policy: Clock})
	for _, id := range ids[:3] {
		mustUnpin(t, p, mustFetch(t, p, id), false)
	}
	mustUnpin(t, p, mustFetch(t, p, ids[3]), false)
	if p.Contains(ids[0]) || !p.Contains(ids[1]) || !p.Contains(ids[2]) {
		t.Fatalf("clock with every bit set must evict the first page loaded: resident %v %v %v",
			p.Contains(ids[0]), p.Contains(ids[1]), p.Contains(ids[2]))
	}
}

func TestAShortAllocatedPageGivesItsFrameBack(t *testing.T) {
	fs := newFaultSource(NewMemSource())
	p := mustPool(t, fs, Options{Frames: 1})
	fs.allocHook = func(pg page.Page) page.Page { return pg[:10] }
	if _, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrShortPage) {
		t.Fatalf("NewPage of a short page gave %v", err)
	}
	fs.allocHook = nil
	f, err := p.NewPage(page.KindHeap)
	if err != nil {
		t.Fatalf("the only frame was lost after a short allocation: %v", err)
	}
	mustUnpin(t, p, f, false)
}

func TestNewPageRefusingAPinnedIDGivesItsFrameBack(t *testing.T) {
	src := NewMemSource()
	p := mustPool(t, src, Options{Frames: 2})
	g, err := p.NewPage(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Free(g.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrPinned) {
		t.Fatalf("NewPage of a pinned id gave %v", err)
	}
	h, err := p.NewPage(page.KindHeap)
	if err != nil {
		t.Fatalf("the frame grabbed for the refused NewPage was not given back: %v", err)
	}
	mustUnpin(t, p, h, false)
	mustUnpin(t, p, g, false)
}

func TestAReallocatedPageIsNeverOverwrittenByItsStaleFrame(t *testing.T) {
	src := NewMemSource()
	p := mustPool(t, src, Options{Frames: 3})
	f, err := p.NewPage(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID()
	f.Page()[page.HeaderSize] = 0x11
	mustUnpin(t, p, f, true)
	if err := src.Free(id); err != nil {
		t.Fatal(err)
	}
	g, err := p.NewPage(page.KindHeap)
	if err != nil || g.ID() != id {
		t.Fatalf("NewPage gave %v, id %d", err, g.ID())
	}
	mustUnpin(t, p, g, false)
	if err := p.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if b := sourceByte(t, src, id); b != 0 {
		t.Fatalf("the stale frame's bytes %02X were written over the reallocated page", b)
	}
}

func TestAFreedFramesLRUKHistoryIsNotInheritedByTheNextPage(t *testing.T) {
	src, ids := seeded(t, 4)
	a, b, c, d := ids[0], ids[1], ids[2], ids[3]
	p := mustPool(t, src, Options{Frames: 2, Policy: LRUK, K: 2})
	for _, id := range []core.PageID{b, b, a, a} {
		mustUnpin(t, p, mustFetch(t, p, id), false)
	}
	if err := p.Free(a); err != nil {
		t.Fatal(err)
	}
	mustUnpin(t, p, mustFetch(t, p, c), false)
	mustUnpin(t, p, mustFetch(t, p, d), false)
	if p.Contains(c) || !p.Contains(b) {
		t.Fatalf("page %d has one access and must go before page %d with two: resident c=%v b=%v",
			c, b, p.Contains(c), p.Contains(b))
	}
}

func TestMemSourceReadHandsOutACopy(t *testing.T) {
	m := NewMemSource()
	p, err := m.Allocate(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Read(p.ID())
	if err != nil {
		t.Fatal(err)
	}
	got[page.HeaderSize] = 0x5A
	again, err := m.Read(p.ID())
	if err != nil {
		t.Fatal(err)
	}
	if again[page.HeaderSize] != 0 {
		t.Fatal("changing the slice Read returned changed the stored page")
	}
}

func TestNewAcceptsExactlyMaxFrames(t *testing.T) {
	p, err := New(NewMemSource(), Options{Frames: MaxFrames})
	if err != nil {
		t.Fatalf("New with exactly MaxFrames gave %v", err)
	}
	if p.Frames() != MaxFrames {
		t.Fatalf("the pool has %d frames, want %d", p.Frames(), MaxFrames)
	}
}
