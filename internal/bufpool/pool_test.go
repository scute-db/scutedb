package bufpool

import (
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
	"github.com/scute-db/scutedb/internal/pagestore"
)

var _ Source = (*pagestore.Store)(nil)
var _ Source = (*MemSource)(nil)

func allPolicies() []Policy { return []Policy{LRU, Clock, LRUK} }

func TestAFullPoolRefusesToFetchWhenEveryFrameIsPinned(t *testing.T) {
	for _, pol := range allPolicies() {
		src, ids := seeded(t, 4)
		p := mustPool(t, src, Options{Frames: 3, Policy: pol})
		var held []*Frame
		for _, id := range ids[:3] {
			held = append(held, mustFetch(t, p, id))
		}
		if _, err := p.Fetch(ids[3]); !errors.Is(err, ErrNoFreeFrame) {
			t.Fatalf("%s: Fetch with every frame pinned gave %v, want ErrNoFreeFrame", pol, err)
		}
		if _, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrNoFreeFrame) {
			t.Fatalf("%s: NewPage with every frame pinned gave %v, want ErrNoFreeFrame", pol, err)
		}
		if src.Len() != 4 {
			t.Fatalf("%s: a refused NewPage still allocated a page in the source", pol)
		}
		for i, f := range held {
			if f.ID() != ids[i] || f.Page()[page.HeaderSize] != byte(i) || f.Pins() != 1 {
				t.Fatalf("%s: pinned frame %d changed under a refused fetch", pol, i)
			}
		}
		mustUnpin(t, p, held[1], false)
		f := mustFetch(t, p, ids[3])
		if p.Contains(ids[1]) || !p.Contains(ids[3]) {
			t.Fatalf("%s: the only unpinned frame was not the one reused", pol)
		}
		mustUnpin(t, p, f, false)
	}
}

func TestFetchCountsHitsMissesAndReads(t *testing.T) {
	src, ids := seeded(t, 3)
	p := mustPool(t, src, Options{Frames: 2})
	for _, id := range []core.PageID{ids[0], ids[0], ids[1], ids[0], ids[2], ids[1]} {
		mustUnpin(t, p, mustFetch(t, p, id), false)
	}
	want := Stats{Hits: 2, Misses: 4, Evictions: 2, Writes: 0, Reads: 4}
	if got := p.Stats(); got != want {
		t.Fatalf("stats %+v, want %+v", got, want)
	}
	if got := p.HitRatio(); got != 2.0/6.0 {
		t.Fatalf("hit ratio %v, want %v", got, 2.0/6.0)
	}
	p.ResetStats()
	if p.Stats() != (Stats{}) || p.HitRatio() != 0 {
		t.Fatalf("ResetStats left %+v", p.Stats())
	}
}

func TestFetchReturnsTheLiveBufferSoChangesAreSeenByTheNextPin(t *testing.T) {
	src, ids := seeded(t, 1)
	p := mustPool(t, src, Options{Frames: 2})
	a := mustFetch(t, p, ids[0])
	b := mustFetch(t, p, ids[0])
	if a != b || a.Pins() != 2 {
		t.Fatalf("two pins of one page gave different frames or %d pins", a.Pins())
	}
	a.Page()[page.HeaderSize] = 0x77
	if b.Page()[page.HeaderSize] != 0x77 {
		t.Fatal("a change through one pin is not visible through the other")
	}
	mustUnpin(t, p, a, true)
	mustUnpin(t, p, b, false)
	if !a.Dirty() {
		t.Fatal("a frame unpinned dirty by one holder and clean by another must stay dirty")
	}
}

func TestWriteBackWritesADirtyPageOnlyWhenItIsEvictedOrFlushed(t *testing.T) {
	src, ids := seeded(t, 3)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2, Mode: WriteBack})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0xAA
	mustUnpin(t, p, f, true)
	if sourceByte(t, src, ids[0]) != 0 || len(fs.writes) != 0 {
		t.Fatal("write-back wrote the page at unpin time")
	}
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	if sourceByte(t, src, ids[0]) != 0 {
		t.Fatal("the page was written while it was still resident")
	}
	mustUnpin(t, p, mustFetch(t, p, ids[2]), false)
	if p.Contains(ids[0]) {
		t.Fatal("the least recently used page should have been evicted")
	}
	if sourceByte(t, src, ids[0]) != 0xAA {
		t.Fatal("evicting a dirty page did not write it")
	}
	if got := p.Stats(); got.Writes != 1 || got.Evictions != 1 {
		t.Fatalf("stats %+v, want 1 write and 1 eviction", got)
	}
	if !reflect.DeepEqual(fs.writes, []core.PageID{ids[0]}) {
		t.Fatalf("writes %v, want only the dirty page", fs.writes)
	}
}

func TestWriteThroughWritesWhenAPageIsUnpinnedDirty(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2, Mode: WriteThrough})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0xBB
	mustUnpin(t, p, f, true)
	if sourceByte(t, src, ids[0]) != 0xBB || f.Dirty() {
		t.Fatal("write-through did not write the page when it was unpinned dirty")
	}
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	if len(fs.writes) != 1 {
		t.Fatalf("clean unpin wrote: %v", fs.writes)
	}
	if err := p.FlushAll(); err != nil || len(fs.writes) != 1 {
		t.Fatalf("FlushAll after write-through wrote again: %v, %v", err, fs.writes)
	}
}

func TestFlushWritesOnlyDirtyPagesAndFlushAllGoesInPageOrder(t *testing.T) {
	src, ids := seeded(t, 5)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 5})
	for _, i := range []int{3, 0, 4, 1} {
		f := mustFetch(t, p, ids[i])
		f.Page()[page.HeaderSize] = byte(0xC0 + i)
		mustUnpin(t, p, f, i != 1)
	}
	if err := p.Flush(ids[1]); err != nil || len(fs.writes) != 0 {
		t.Fatalf("Flush of a clean page wrote: %v %v", err, fs.writes)
	}
	if err := p.Flush(ids[2]); err != nil || len(fs.writes) != 0 {
		t.Fatalf("Flush of a page that is not resident: %v %v", err, fs.writes)
	}
	if err := p.Flush(ids[3]); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(ids[3]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fs.writes, []core.PageID{ids[3]}) {
		t.Fatalf("writes %v, want one write of page %d", fs.writes, ids[3])
	}
	if err := p.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if want := []core.PageID{ids[3], ids[0], ids[4]}; !reflect.DeepEqual(fs.writes, want) {
		t.Fatalf("writes %v, want %v", fs.writes, want)
	}
	if sourceByte(t, src, ids[1]) != 1 {
		t.Fatal("a page unpinned clean was written even though it was changed")
	}
}

func TestFlushOfAPinnedPageWritesItsCurrentBytes(t *testing.T) {
	src, ids := seeded(t, 1)
	p := mustPool(t, src, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x42
	mustUnpin(t, p, f, true)
	f = mustFetch(t, p, ids[0])
	if err := p.Flush(ids[0]); err != nil {
		t.Fatal(err)
	}
	if sourceByte(t, src, ids[0]) != 0x42 || f.Dirty() || f.Pins() != 1 {
		t.Fatal("Flush of a pinned page did not write it, or disturbed the pin")
	}
	mustUnpin(t, p, f, false)
}

func TestPinnedFramesAreNeverEvicted(t *testing.T) {
	for _, pol := range allPolicies() {
		src, ids := seeded(t, 40)
		p := mustPool(t, src, Options{Frames: 4, Policy: pol})
		mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
		keep := mustFetch(t, p, ids[0])
		if p.Stats().Hits != 1 {
			t.Fatalf("%s: the second fetch was not a cache hit", pol)
		}
		keep.Page()[page.HeaderSize] = 0xEE
		for round := 0; round < 3; round++ {
			for _, id := range ids[1:] {
				mustUnpin(t, p, mustFetch(t, p, id), false)
			}
		}
		if !p.Contains(ids[0]) || keep.ID() != ids[0] || keep.Page()[page.HeaderSize] != 0xEE {
			t.Fatalf("%s: a pinned frame was evicted or reused", pol)
		}
		mustUnpin(t, p, keep, true)
		if p.Pinned() != 0 {
			t.Fatalf("%s: %d frames still pinned", pol, p.Pinned())
		}
	}
}

func TestFreeRefusesAPinnedPageAndDropsAnUnpinnedOne(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	f := mustFetch(t, p, ids[0])
	if err := p.Free(ids[0]); !errors.Is(err, ErrPinned) {
		t.Fatalf("Free of a pinned page gave %v, want ErrPinned", err)
	}
	if src.Len() != 2 {
		t.Fatal("a refused Free reached the source")
	}
	f.Page()[page.HeaderSize] = 0x99
	mustUnpin(t, p, f, true)
	if err := p.Free(ids[0]); err != nil {
		t.Fatal(err)
	}
	if p.Contains(ids[0]) || p.Resident() != 0 || src.Len() != 1 {
		t.Fatalf("after Free: contains %v, resident %d, source holds %d", p.Contains(ids[0]), p.Resident(), src.Len())
	}
	if len(fs.writes) != 0 {
		t.Fatal("freeing a dirty page wrote it first; its bytes are garbage now")
	}
	if err := p.Free(ids[1]); err != nil {
		t.Fatalf("Free of a page that is not resident: %v", err)
	}
	if err := p.Free(ids[1]); !errors.Is(err, ErrNoSuchPage) {
		t.Fatalf("double Free gave %v, want the source's error", err)
	}
}

func TestAFailedFreeKeepsTheFrame(t *testing.T) {
	src, ids := seeded(t, 1)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x55
	mustUnpin(t, p, f, true)
	fs.failFree = true
	if err := p.Free(ids[0]); !errors.Is(err, errInjected) {
		t.Fatalf("Free gave %v", err)
	}
	if !p.Contains(ids[0]) || !f.Dirty() {
		t.Fatal("a failed Free dropped the frame and its unwritten change")
	}
}

func TestUnpinRejectsFramesThatAreNotPinnedHere(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 1})
	other := mustPool(t, src, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	g := mustFetch(t, other, ids[0])
	if err := p.Unpin(g, false); !errors.Is(err, ErrNotPinned) {
		t.Errorf("Unpin of another pool's frame gave %v", err)
	}
	if err := p.Unpin(nil, false); !errors.Is(err, ErrNotPinned) {
		t.Errorf("Unpin(nil) gave %v", err)
	}
	mustUnpin(t, p, f, false)
	if err := p.Unpin(f, false); !errors.Is(err, ErrNotPinned) {
		t.Errorf("a second Unpin gave %v", err)
	}
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	if err := p.Unpin(f, false); !errors.Is(err, ErrNotPinned) {
		t.Errorf("Unpin of a frame whose page was evicted gave %v", err)
	}
	if f.Pins() != 0 {
		t.Errorf("pin count went to %d", f.Pins())
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	src := NewMemSource()
	cases := []struct {
		name string
		src  Source
		opts Options
	}{
		{"no source", nil, Options{Frames: 1}},
		{"zero frames", src, Options{Frames: 0}},
		{"negative frames", src, Options{Frames: -4}},
		{"more frames than the limit", src, Options{Frames: MaxFrames + 1}},
		{"a frame count whose size would overflow", src, Options{Frames: math.MaxInt}},
		{"negative K", src, Options{Frames: 2, Policy: LRUK, K: -1}},
		{"unknown policy", src, Options{Frames: 2, Policy: 9}},
		{"unknown mode", src, Options{Frames: 2, Mode: 7}},
	}
	for _, c := range cases {
		if _, err := New(c.src, c.opts); !errors.Is(err, ErrBadOptions) {
			t.Errorf("%s: New gave %v, want ErrBadOptions", c.name, err)
		}
	}
	p, err := New(src, Options{Frames: 3, Policy: LRUK})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Options(); got.K != DefaultK || got.Frames != 3 || p.Frames() != 3 {
		t.Fatalf("options %+v, want K defaulted to %d", got, DefaultK)
	}
}

func TestPolicyAndModeNames(t *testing.T) {
	names := map[string]string{
		LRU.String(): "LRU", Clock.String(): "Clock", LRUK.String(): "LRU-K", Policy(9).String(): "policy(9)",
		WriteBack.String(): "write-back", WriteThrough.String(): "write-through", Mode(5).String(): "mode(5)",
	}
	for got, want := range names {
		if got != want {
			t.Errorf("name %q, want %q", got, want)
		}
	}
}

func TestNewPageAllocatesThroughTheSourceAndComesBackPinned(t *testing.T) {
	src := NewMemSource()
	p := mustPool(t, src, Options{Frames: 2})
	f, err := p.NewPage(page.KindBTreeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if f.Pins() != 1 || f.Dirty() || f.Page().Kind() != page.KindBTreeLeaf || f.Page().ID() != f.ID() {
		t.Fatalf("new frame: pins %d dirty %v kind %s id %d/%d", f.Pins(), f.Dirty(), f.Page().Kind(), f.Page().ID(), f.ID())
	}
	if src.Len() != 1 || !p.Contains(f.ID()) {
		t.Fatal("the page was not allocated in the source or not installed in the pool")
	}
	if got := p.Stats(); got.Hits != 0 || got.Misses != 0 || got.Reads != 0 {
		t.Fatalf("NewPage counted as a fetch: %+v", got)
	}
	mustUnpin(t, p, f, false)
}

func TestNewPageReplacesAStaleUnpinnedFrameAndRefusesAPinnedOne(t *testing.T) {
	src := NewMemSource()
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 3})
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
	if err != nil {
		t.Fatal(err)
	}
	if g.ID() != id || g.Page()[page.HeaderSize] != 0 || g.Dirty() || p.Resident() != 1 {
		t.Fatalf("reallocated page %d kept stale bytes %02X, dirty %v, resident %d", g.ID(), g.Page()[page.HeaderSize], g.Dirty(), p.Resident())
	}
	if err := src.Free(id); err != nil {
		t.Fatal(err)
	}
	if _, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrPinned) {
		t.Fatalf("NewPage of a page id that is still pinned gave %v, want ErrPinned", err)
	}
	if p.Pinned() != 1 || p.Resident() != 1 {
		t.Fatalf("pinned %d resident %d after the refusal", p.Pinned(), p.Resident())
	}
	mustUnpin(t, p, g, false)
}

func TestAPageWhoseHeaderNamesAnotherPageIsRefused(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	fs.readHook = func(id core.PageID, p page.Page) page.Page {
		if id == ids[1] {
			p.SetID(ids[0])
		}
		return p
	}
	p := mustPool(t, fs, Options{Frames: 2})
	if _, err := p.Fetch(ids[1]); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("Fetch of a page whose header lies gave %v, want ErrIDMismatch", err)
	}
	if p.Resident() != 0 || p.Pinned() != 0 {
		t.Fatal("the refused page was installed")
	}

	f := mustFetch(t, p, ids[0])
	f.Page().SetID(ids[1])
	if err := p.Unpin(f, true); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(ids[0]); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("Flush of a frame whose header was changed gave %v, want ErrIDMismatch", err)
	}
	if sourceByte(t, src, ids[1]) != 1 {
		t.Fatal("the flush overwrote the page the header pointed to")
	}

	fs.readHook = func(id core.PageID, p page.Page) page.Page { return p[:100] }
	if _, err := p.Fetch(ids[1]); !errors.Is(err, ErrShortPage) {
		t.Fatalf("Fetch of a short page gave %v, want ErrShortPage", err)
	}
	fs.allocHook = func(p page.Page) page.Page { return p[:10] }
	if _, err := p.NewPage(page.KindHeap); !errors.Is(err, ErrShortPage) {
		t.Fatalf("NewPage of a short page gave %v, want ErrShortPage", err)
	}
}

func TestAFailedEvictionWriteKeepsThePageDirtyAndResident(t *testing.T) {
	src, ids := seeded(t, 3)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0xD1
	mustUnpin(t, p, f, true)

	fs.failWrite[ids[0]] = true
	if _, err := p.Fetch(ids[1]); !errors.Is(err, errInjected) {
		t.Fatalf("Fetch that had to evict a dirty page gave %v", err)
	}
	if !p.Contains(ids[0]) || !f.Dirty() || f.Page()[page.HeaderSize] != 0xD1 || p.Pinned() != 0 {
		t.Fatal("a failed eviction write lost the dirty page")
	}
	if p.Stats().Evictions != 0 {
		t.Fatal("a failed eviction was counted")
	}

	delete(fs.failWrite, ids[0])
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	if sourceByte(t, src, ids[0]) != 0xD1 {
		t.Fatal("the retried eviction did not write the page")
	}
}

func TestAFailedReadLeavesNoPinAndFreesTheFrame(t *testing.T) {
	src, ids := seeded(t, 3)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2})
	mustUnpin(t, p, mustFetch(t, p, ids[0]), false)
	fs.failRead[ids[1]] = true
	if _, err := p.Fetch(ids[1]); !errors.Is(err, errInjected) {
		t.Fatalf("Fetch gave %v", err)
	}
	if p.Pinned() != 0 || p.Resident() != 1 || p.Contains(ids[1]) {
		t.Fatalf("after a failed read: pinned %d, resident %d", p.Pinned(), p.Resident())
	}
	if _, err := p.Fetch(core.PageID(999)); !errors.Is(err, ErrNoSuchPage) {
		t.Fatalf("Fetch of a page the source does not have gave %v", err)
	}
	a := mustFetch(t, p, ids[0])
	b := mustFetch(t, p, ids[2])
	if p.Pinned() != 2 {
		t.Fatal("frames freed by failed reads were not reused")
	}
	mustUnpin(t, p, a, false)
	mustUnpin(t, p, b, false)
}

func TestAFailedAllocationLeavesNoPin(t *testing.T) {
	fs := newFaultSource(NewMemSource())
	p := mustPool(t, fs, Options{Frames: 1})
	fs.failAlloc = true
	if _, err := p.NewPage(page.KindHeap); !errors.Is(err, errInjected) {
		t.Fatalf("NewPage gave %v", err)
	}
	fs.failAlloc = false
	f, err := p.NewPage(page.KindHeap)
	if err != nil {
		t.Fatalf("the frame was not returned after a failed allocation: %v", err)
	}
	mustUnpin(t, p, f, false)
}

func TestAFailedWriteThroughStillReleasesThePin(t *testing.T) {
	src, ids := seeded(t, 2)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 1, Mode: WriteThrough})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x3C
	fs.failWrite[ids[0]] = true
	if err := p.Unpin(f, true); !errors.Is(err, errInjected) {
		t.Fatalf("Unpin gave %v", err)
	}
	if f.Pins() != 0 || !f.Dirty() {
		t.Fatalf("after a failed write-through: pins %d dirty %v", f.Pins(), f.Dirty())
	}
	delete(fs.failWrite, ids[0])
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	if sourceByte(t, src, ids[0]) != 0x3C {
		t.Fatal("the page that failed to write through was not written on eviction")
	}
}

func TestFlushAllReportsEveryFailureAndWritesTheRest(t *testing.T) {
	src, ids := seeded(t, 3)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 3})
	for i, id := range ids {
		f := mustFetch(t, p, id)
		f.Page()[page.HeaderSize] = byte(0xA0 + i)
		mustUnpin(t, p, f, true)
	}
	fs.failWrite[ids[0]] = true
	fs.failWrite[ids[2]] = true
	err := p.FlushAll()
	if !errors.Is(err, errInjected) || strings.Count(err.Error(), errInjected.Error()) != 2 {
		t.Fatalf("FlushAll gave %v, want both failures", err)
	}
	if sourceByte(t, src, ids[1]) != 0xA1 {
		t.Fatal("FlushAll stopped at the first failure")
	}
}

func TestTheVerifiedMarkIsClearedWhenAPageIsReloaded(t *testing.T) {
	src, ids := seeded(t, 2)
	p := mustPool(t, src, Options{Frames: 1})
	f := mustFetch(t, p, ids[0])
	if f.Verified() {
		t.Fatal("a freshly read page is already marked verified")
	}
	f.MarkVerified()
	mustUnpin(t, p, f, false)
	f = mustFetch(t, p, ids[0])
	if !f.Verified() {
		t.Fatal("a cache hit lost the verified mark")
	}
	mustUnpin(t, p, f, false)
	mustUnpin(t, p, mustFetch(t, p, ids[1]), false)
	f = mustFetch(t, p, ids[0])
	if f.Verified() {
		t.Fatal("a page read again from the source kept an old verified mark")
	}
	mustUnpin(t, p, f, false)
}

func TestThePoolRunsOnARealPageStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.db")
	s, err := pagestore.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	p := mustPool(t, s, Options{Frames: 3, Policy: LRUK})
	var ids []core.PageID
	for i := 0; i < 10; i++ {
		f, err := p.NewPage(page.KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		copy(f.Page()[page.HeaderSize:], []byte{byte(i), byte(i * 3)})
		ids = append(ids, f.ID())
		mustUnpin(t, p, f, true)
	}
	if err := p.FlushAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = pagestore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p = mustPool(t, s, Options{Frames: 2, Policy: Clock})
	for i, id := range ids {
		f := mustFetch(t, p, id)
		if f.Page()[page.HeaderSize] != byte(i) || f.Page()[page.HeaderSize+1] != byte(i*3) {
			t.Fatalf("page %d came back with % 02X", id, f.Page()[page.HeaderSize:page.HeaderSize+2])
		}
		mustUnpin(t, p, f, false)
	}
	if err := p.Free(ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Fetch(ids[0]); !errors.Is(err, pagestore.ErrNotAllocated) {
		t.Fatalf("Fetch of a freed page gave %v, want pagestore.ErrNotAllocated", err)
	}
}

func TestMemSourceRefusesShortAndUnknownPages(t *testing.T) {
	m := NewMemSource()
	p, err := m.Allocate(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Write(p[:100]); !errors.Is(err, ErrShortPage) {
		t.Errorf("Write of a short page gave %v", err)
	}
	if err := m.Write(page.New(42, page.KindHeap)); !errors.Is(err, ErrNoSuchPage) {
		t.Errorf("Write of a page never allocated gave %v", err)
	}
	if err := m.Free(42); !errors.Is(err, ErrNoSuchPage) {
		t.Errorf("Free of a page never allocated gave %v", err)
	}
	p[page.HeaderSize] = 9
	back, _ := m.Read(p.ID())
	if back[page.HeaderSize] != 0 {
		t.Error("changing the slice Allocate returned changed the stored page")
	}
	if err := m.Free(p.ID()); err != nil {
		t.Fatal(err)
	}
	again, err := m.Allocate(page.KindMeta)
	if err != nil || again.ID() != p.ID() || again.Kind() != page.KindMeta || m.Len() != 1 {
		t.Fatalf("a freed id was not reused: %v %d", err, again.ID())
	}
}

func TestWriteThroughRetriesAFailedWriteAtTheNextUnpin(t *testing.T) {
	src, ids := seeded(t, 1)
	fs := newFaultSource(src)
	p := mustPool(t, fs, Options{Frames: 2, Mode: WriteThrough})
	f := mustFetch(t, p, ids[0])
	f.Page()[page.HeaderSize] = 0x6B
	fs.failWrite[ids[0]] = true
	if err := p.Unpin(f, true); !errors.Is(err, errInjected) {
		t.Fatalf("Unpin gave %v", err)
	}
	delete(fs.failWrite, ids[0])
	f = mustFetch(t, p, ids[0])
	mustUnpin(t, p, f, false)
	if f.Dirty() || sourceByte(t, src, ids[0]) != 0x6B {
		t.Fatal("a clean unpin of a page whose write-through failed did not retry the write")
	}
}
