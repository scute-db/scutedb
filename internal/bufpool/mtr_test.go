package bufpool

import (
	"bytes"
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func mtrFetch(t *testing.T, m *MTR, id core.PageID) *Frame {
	t.Helper()
	f, err := m.Fetch(id)
	if err != nil {
		t.Fatalf("m.Fetch(%d): %v", id, err)
	}
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func latchFree(f *Frame) bool {
	if !f.latch.TryLock() {
		return false
	}
	f.latch.Unlock()
	return true
}

func TestACommittedMTRMarksOnlyTheChangedPagesDirtyAndReleasesEverything(t *testing.T) {
	src, ids := seeded(t, 4)
	p := mustPool(t, src, Options{Frames: 4})
	m := p.Begin()
	a, b, c, d := mtrFetch(t, m, ids[0]), mtrFetch(t, m, ids[1]), mtrFetch(t, m, ids[2]), mtrFetch(t, m, ids[3])
	must(t, m.WLatch(a))
	must(t, m.WLatch(b))
	must(t, m.RLatch(c))
	a.Page()[page.HeaderSize] = 0xA1
	if got := m.Modified(); len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("Modified() = %v, want only page %d", got, ids[0])
	}
	if p.Pinned() != 4 || m.Held() != 4 || !m.Holds(ids[3]) {
		t.Fatalf("pinned %d, held %d", p.Pinned(), m.Held())
	}
	must(t, m.Commit())
	if !a.Dirty() || b.Dirty() || c.Dirty() || d.Dirty() {
		t.Fatalf("dirty after commit: changed %v, latched but unchanged %v, read %v, pinned only %v", a.Dirty(), b.Dirty(), c.Dirty(), d.Dirty())
	}
	if p.Pinned() != 0 || m.Held() != 0 || m.Holds(ids[0]) {
		t.Fatalf("after commit %d frames are pinned and the MTR holds %d", p.Pinned(), m.Held())
	}
	for _, f := range []*Frame{a, b, c, d} {
		if !latchFree(f) {
			t.Fatalf("page %d is still latched after commit", f.ID())
		}
	}
	must(t, p.FlushAll())
	if sourceByte(t, src, ids[0]) != 0xA1 {
		t.Fatal("the committed change did not reach the source")
	}
}

func TestAnAbortedMTRRestoresEveryBeforeImageAndFreesItsNewPages(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 5})
	f := mustFetch(t, p, ids[1])
	f.Page()[page.HeaderSize] = 0x77
	mustUnpin(t, p, f, true)

	m := p.Begin()
	a, b := mtrFetch(t, m, ids[0]), mtrFetch(t, m, ids[1])
	must(t, m.WLatch(a))
	must(t, m.WLatch(b))
	a.Page()[page.HeaderSize] = 0xEE
	b.Page()[page.HeaderSize+1] = 0xEE
	n, err := m.NewPage(page.KindHeap)
	must(t, err)
	must(t, m.WLatch(n))
	n.Page()[page.HeaderSize] = 0x99
	id, err := m.Alloc(page.KindHeap)
	must(t, err)
	if src.Len() != 4 {
		t.Fatalf("source holds %d pages, want 4", src.Len())
	}
	must(t, m.Abort())
	if a.Page()[page.HeaderSize] != 0 || b.Page()[page.HeaderSize] != 0x77 || b.Page()[page.HeaderSize+1] != 0 {
		t.Fatal("abort did not restore the before-images")
	}
	if a.Dirty() || !b.Dirty() {
		t.Fatalf("abort changed the dirty flags: clean page dirty %v, dirty page dirty %v", a.Dirty(), b.Dirty())
	}
	if src.Len() != 2 || p.Contains(n.ID()) || p.Contains(id) {
		t.Fatalf("abort left the pages it allocated: source holds %d", src.Len())
	}
	if p.Pinned() != 0 || !latchFree(a) || !latchFree(b) {
		t.Fatal("abort left a pin or a latch")
	}
}

func TestAnAbortPutsBackTheVerifiedMarkTheFrameHadBefore(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 2})
	m := p.Begin()
	a, b := mtrFetch(t, m, ids[0]), mtrFetch(t, m, ids[1])
	a.MarkVerified()
	must(t, m.WLatch(a))
	must(t, m.WLatch(b))
	a.Page()[page.HeaderSize] = 1
	b.Page()[page.HeaderSize] = 1
	a.verified.Store(false)
	b.MarkVerified()
	must(t, m.Abort())
	if !a.Verified() || b.Verified() {
		t.Fatalf("after abort verified = %v and %v, want true and false as before the MTR", a.Verified(), b.Verified())
	}
}

func TestReleaseLetsGoOfAnUnchangedPageAndRefusesAChangedOne(t *testing.T) {
	src, ids := seeded(t, 3)
	p := mustPool(t, src, Options{Frames: 3})
	m := p.Begin()
	a, b, c := mtrFetch(t, m, ids[0]), mtrFetch(t, m, ids[1]), mtrFetch(t, m, ids[2])
	must(t, m.WLatch(a))
	must(t, m.RLatch(b))
	must(t, m.WLatch(c))
	c.Page()[page.HeaderSize] = 0xC1
	must(t, m.Release(a))
	must(t, m.Release(b))
	if err := m.Release(c); !errors.Is(err, ErrModified) {
		t.Fatalf("Release of a changed page gave %v, want ErrModified", err)
	}
	if m.Holds(ids[0]) || m.Holds(ids[1]) || !m.Holds(ids[2]) || m.Held() != 1 || p.Pinned() != 1 {
		t.Fatalf("after releases the MTR holds %d pages, %d pinned", m.Held(), p.Pinned())
	}
	if !latchFree(a) || !latchFree(b) || latchFree(c) {
		t.Fatal("Release did not free exactly the released latches")
	}
	d := mtrFetch(t, m, ids[0])
	must(t, m.Release(d))
	if err := m.Release(a); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("a second Release gave %v, want ErrNotHeld", err)
	}
	must(t, m.Commit())
	if a.Dirty() || !c.Dirty() {
		t.Fatal("commit after a release dirtied the wrong pages")
	}
}

func TestWLatchAfterRLatchIsRefusedInsteadOfDeadlocking(t *testing.T) {
	src, ids := seeded(t, 1)
	p := mustPool(t, src, Options{Frames: 1})
	m := p.Begin()
	f := mtrFetch(t, m, ids[0])
	must(t, m.RLatch(f))
	must(t, m.RLatch(f))
	if err := m.WLatch(f); !errors.Is(err, ErrUpgrade) {
		t.Fatalf("WLatch over a read latch gave %v, want ErrUpgrade", err)
	}
	must(t, m.Commit())
	g := p.Begin()
	f = mtrFetch(t, g, ids[0])
	must(t, g.WLatch(f))
	f.Page()[page.HeaderSize] = 5
	must(t, g.WLatch(f))
	must(t, g.RLatch(f))
	if got := g.Modified(); len(got) != 1 {
		t.Fatalf("a second WLatch replaced the before-image: Modified() = %v", got)
	}
	must(t, g.Abort())
	if f.Page()[page.HeaderSize] != 0 {
		t.Fatal("abort restored a later before-image instead of the first")
	}
}

func TestFetchingAPageTheMTRAlreadyHoldsReturnsItWithoutAnotherPin(t *testing.T) {
	src, ids := seeded(t, 1)
	p := mustPool(t, src, Options{Frames: 1})
	m := p.Begin()
	a := mtrFetch(t, m, ids[0])
	b := mtrFetch(t, m, ids[0])
	if a != b || a.Pins() != 1 || m.Held() != 1 {
		t.Fatalf("fetching twice gave different frames or %d pins", a.Pins())
	}
	must(t, m.Commit())
	if a.Pins() != 0 {
		t.Fatal("a page fetched twice by one MTR kept a pin")
	}
}

func TestAnMTRRefusesFramesItDoesNotHold(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 3})
	other := mustPool(t, src, Options{Frames: 1})
	m := p.Begin()
	outside := mustFetch(t, p, ids[0])
	foreign := mustFetch(t, other, ids[1])
	mtrFetch(t, m, ids[1])
	for _, f := range []*Frame{nil, outside, foreign} {
		for name, op := range map[string]func(*Frame) error{"WLatch": m.WLatch, "RLatch": m.RLatch, "Release": m.Release} {
			if err := op(f); !errors.Is(err, ErrNotHeld) {
				t.Fatalf("%s of a frame the MTR does not hold gave %v", name, err)
			}
		}
	}
	must(t, m.Commit())
	mustUnpin(t, p, outside, false)
	mustUnpin(t, other, foreign, false)
}

func TestAFinishedMTRRefusesEveryCall(t *testing.T) {
	for _, end := range []string{"commit", "abort"} {
		src, ids := seeded(t, 1)
		p := mustPool(t, src, Options{Frames: 2})
		m := p.Begin()
		f := mtrFetch(t, m, ids[0])
		if end == "commit" {
			must(t, m.Commit())
		} else {
			must(t, m.Abort())
		}
		calls := map[string]error{
			"Commit":  m.Commit(),
			"Abort":   m.Abort(),
			"Free":    m.Free(ids[0]),
			"WLatch":  m.WLatch(f),
			"RLatch":  m.RLatch(f),
			"Release": m.Release(f),
			"SetUndo": m.SetUndo(1, 2, nil),
		}
		_, calls["Fetch"] = m.Fetch(ids[0])
		_, calls["NewPage"] = m.NewPage(page.KindHeap)
		_, calls["Alloc"] = m.Alloc(page.KindHeap)
		for name, err := range calls {
			if !errors.Is(err, ErrMTRDone) {
				t.Fatalf("after %s, %s gave %v, want ErrMTRDone", end, name, err)
			}
		}
		if src.Len() != 1 || p.Pinned() != 0 {
			t.Fatalf("after %s a refused call still allocated or pinned", end)
		}
		if m.Pool() != p {
			t.Fatal("Pool() lost its pool")
		}
	}
}

func TestSetUndoKeepsOneRecordAndCopiesItsPayload(t *testing.T) {
	p := mustPool(t, NewMemSource(), Options{Frames: 1})
	m := p.Begin()
	if _, ok := m.Undo(); ok {
		t.Fatal("a new MTR already has an undo record")
	}
	payload := []byte{1, 2, 3}
	must(t, m.SetUndo(7, 99, payload))
	payload[0] = 9
	if err := m.SetUndo(8, 100, nil); !errors.Is(err, ErrUndoSet) {
		t.Fatalf("a second SetUndo gave %v, want ErrUndoSet", err)
	}
	u, ok := m.Undo()
	if !ok || u.Txn != 7 || u.PrevLSN != 99 || !bytes.Equal(u.Payload, []byte{1, 2, 3}) {
		t.Fatalf("Undo() = %+v %v", u, ok)
	}
	u.Payload[1] = 9
	if again, _ := m.Undo(); again.Payload[1] != 2 {
		t.Fatal("Undo() handed out the stored payload itself")
	}
	must(t, m.Commit())
}

func TestFreeInAnMTRHappensAtCommitAndNeverAtAbort(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	m := p.Begin()
	f := mtrFetch(t, m, ids[0])
	must(t, m.Free(ids[0]))
	if src.Len() != 2 {
		t.Fatal("m.Free freed the page before commit")
	}
	must(t, m.Abort())
	if src.Len() != 2 || f.Pins() != 0 {
		t.Fatal("an aborted MTR freed a page")
	}
	m = p.Begin()
	mtrFetch(t, m, ids[0])
	must(t, m.Free(ids[0]))
	must(t, m.Free(ids[1]))
	fs.failFree = true
	if err := m.Commit(); !errors.Is(err, errInjected) {
		t.Fatalf("commit whose frees fail gave %v", err)
	}
	fs.failFree = false
	m = p.Begin()
	mtrFetch(t, m, ids[0])
	must(t, m.Free(ids[0]))
	must(t, m.Free(ids[1]))
	must(t, m.Commit())
	if src.Len() != 0 || p.Resident() != 0 {
		t.Fatalf("after commit the source holds %d pages and the pool %d", src.Len(), p.Resident())
	}
}

func TestCommitReportsAFailedWriteThroughAndStillReleasesEverything(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2, Mode: WriteThrough})
	m := p.Begin()
	a, b := mtrFetch(t, m, ids[0]), mtrFetch(t, m, ids[1])
	must(t, m.WLatch(a))
	must(t, m.WLatch(b))
	a.Page()[page.HeaderSize] = 0x10
	b.Page()[page.HeaderSize] = 0x20
	fs.failWrite[ids[0]] = true
	if err := m.Commit(); !errors.Is(err, errInjected) {
		t.Fatalf("commit with a failing write-through gave %v", err)
	}
	if p.Pinned() != 0 || !latchFree(a) || !a.Dirty() || b.Dirty() || sourceByte(t, src, ids[1]) != 0x20 {
		t.Fatal("a failed write-through stopped the commit from releasing or writing the rest")
	}
}

func TestAbortReportsAFailedFreeOfAPageItAllocated(t *testing.T) {
	fs := newFaultSource(NewMemSource())
	p := mustPool(t, fs, Options{Frames: 2})
	m := p.Begin()
	if _, err := m.NewPage(page.KindHeap); err != nil {
		t.Fatal(err)
	}
	fs.failFree = true
	if err := m.Abort(); !errors.Is(err, errInjected) {
		t.Fatalf("abort whose free fails gave %v", err)
	}
	if p.Pinned() != 0 {
		t.Fatal("a failed free left a pin")
	}
}

func TestAnMTRCallThatFailsKeepsWhatTheMTRAlreadyHolds(t *testing.T) {
	src, ids := seeded(t, 3)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	m := p.Begin()
	a := mtrFetch(t, m, ids[0])
	must(t, m.WLatch(a))
	a.Page()[page.HeaderSize] = 0x31
	fs.failRead[ids[1]] = true
	if _, err := m.Fetch(ids[1]); !errors.Is(err, errInjected) {
		t.Fatalf("m.Fetch gave %v", err)
	}
	fs.failAlloc = true
	if _, err := m.NewPage(page.KindHeap); !errors.Is(err, errInjected) {
		t.Fatalf("m.NewPage gave %v", err)
	}
	if _, err := m.Alloc(page.KindHeap); !errors.Is(err, errInjected) {
		t.Fatalf("m.Alloc gave %v", err)
	}
	if m.Held() != 1 || a.Pins() != 1 {
		t.Fatal("a failed call disturbed the pages the MTR already held")
	}
	mtrFetch(t, m, ids[2])
	if _, err := m.NewPage(page.KindHeap); !errors.Is(err, ErrNoFreeFrame) {
		t.Fatalf("m.NewPage in a full pool gave %v", err)
	}
	must(t, m.Abort())
	if a.Page()[page.HeaderSize] != 0 || p.Pinned() != 0 || src.Len() != 3 {
		t.Fatal("abort after failed calls did not restore the pool")
	}
}

func TestAllocHandsOutAPageWithoutHoldingIt(t *testing.T) {
	src := NewMemSource()
	p := mustPool(t, src, Options{Frames: 1})
	m := p.Begin()
	id, err := m.Alloc(page.KindHeap)
	must(t, err)
	if m.Held() != 0 || p.Pinned() != 0 || src.Len() != 1 {
		t.Fatalf("Alloc held %d pages, pinned %d", m.Held(), p.Pinned())
	}
	f := mtrFetch(t, m, id)
	must(t, m.WLatch(f))
	f.Page()[page.HeaderSize] = 0x42
	must(t, m.Commit())
	must(t, p.FlushAll())
	if sourceByte(t, src, id) != 0x42 {
		t.Fatal("the allocated page did not keep its change")
	}
}
