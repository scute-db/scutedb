package pagestore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func TestUseAfterFreeIsCaught(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	p := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(p.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(p.ID()); !errors.Is(err, ErrNotAllocated) {
		t.Errorf("reading a freed page gave %v, want ErrNotAllocated", err)
	}
	if err := s.Write(p); !errors.Is(err, ErrNotAllocated) {
		t.Errorf("writing a freed page gave %v, want ErrNotAllocated", err)
	}
	if err := s.SetRoot(p.ID()); !errors.Is(err, ErrNotAllocated) {
		t.Errorf("making a freed page the root gave %v, want ErrNotAllocated", err)
	}
}

func TestTheRootCannotBeFreedUntilItIsReplaced(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	old := mustAlloc(t, s)
	replacement := mustAlloc(t, s)
	if err := s.SetRoot(old.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(old.ID()); !errors.Is(err, ErrFreeRoot) {
		t.Fatalf("freeing the current root gave %v, want ErrFreeRoot", err)
	}
	if err := s.SetRoot(replacement.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(old.ID()); err != nil {
		t.Errorf("freeing the old root after replacing it gave %v, want nil", err)
	}
}

func TestFreeListPagesCannotBeFreedOrUsed(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	a := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(a.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	lists := s.ListPages()
	if len(lists) != 1 {
		t.Fatalf("expected one free-list page, got %v", lists)
	}
	if err := s.Free(lists[0]); !errors.Is(err, ErrNotAllocated) {
		t.Errorf("freeing a free-list page gave %v, want ErrNotAllocated", err)
	}
	if _, err := s.Read(lists[0]); !errors.Is(err, ErrNotAllocated) {
		t.Errorf("reading a free-list page through the store gave %v, want ErrNotAllocated", err)
	}
}

func TestFreeListSpansSeveralPages(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)

	n := FreeListCapacity + 200
	ids := make([]core.PageID, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, mustAlloc(t, s).ID())
	}
	for _, id := range ids {
		if err := s.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if got := len(s.ListPages()); got != 2 {
		t.Fatalf("%d free pages need 2 free-list pages of %d entries, got %d",
			len(s.FreePages()), FreeListCapacity, got)
	}
	wantFree := len(s.FreePages())
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if err := back.Verify(); err != nil {
		t.Fatalf("Verify after reopening a two-page free list: %v", err)
	}
	if len(back.FreePages()) != wantFree {
		t.Errorf("after a restart %d pages are free, want %d", len(back.FreePages()), wantFree)
	}
}

func TestFreeListPagesAreRecycledSoTheFileStopsGrowing(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()

	keep := mustAlloc(t, s)
	if err := s.SetRoot(keep.ID()); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 200; round++ {
		p := mustAlloc(t, s)
		if err := s.Free(p.ID()); err != nil {
			t.Fatal(err)
		}
		if err := s.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	if next := s.Meta().NextPage; next > 8 {
		t.Errorf("after 200 allocate-free-checkpoint rounds, %d pages have been handed out; "+
			"free-list pages are leaking instead of being recycled", next-1)
	}
	if s.Meta().FilePages != ChunkPages {
		t.Errorf("the file grew to %d pages over a workload that never holds more than a few", s.Meta().FilePages)
	}
}

func TestAFailedFsyncPoisonsTheStore(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	durable := s.Durable()

	p, err := s.Allocate(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}

	mf.failAtSync = mf.syncs + 1
	if err := s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a checkpoint whose fsync failed gave %v, want ErrPoisoned", err)
	}
	if _, err := s.Allocate(page.KindHeap); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Allocate after a failed checkpoint gave %v, want ErrPoisoned", err)
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		t.Errorf("retrying the checkpoint gave %v, want ErrPoisoned; after a failed fsync nothing is known", err)
	}
	if err := s.Close(); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Close of a poisoned store gave %v, want ErrPoisoned so the failure is not swallowed", err)
	}

	mf.keepNothing()
	back, err := load(page.NewFile(mf))
	if err != nil {
		t.Fatalf("reopening after the failed checkpoint: %v", err)
	}
	if back.Durable() != durable {
		t.Errorf("after recovery the durable meta is %+v, want the last good checkpoint %+v", back.Durable(), durable)
	}
	if err := back.Verify(); err != nil {
		t.Errorf("Verify after recovering from a failed fsync: %v", err)
	}
}

func TestPowerLossKeepsOnlyWhatWasCheckpointed(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Allocate(page.KindHeap)
	if err != nil {
		t.Fatal(err)
	}
	copy(p[page.HeaderSize:], []byte("durable"))
	if err := s.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		q, err := s.Allocate(page.KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		copy(q[page.HeaderSize:], []byte("lost"))
		if err := s.Write(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	mf.keepNothing()

	back, err := load(page.NewFile(mf))
	if err != nil {
		t.Fatalf("reopening after a power loss: %v", err)
	}
	if err := back.Verify(); err != nil {
		t.Fatalf("Verify after a power loss: %v", err)
	}
	if back.Root() != p.ID() || back.Meta().NextPage != p.ID()+1 {
		t.Fatalf("after a power loss the root is %d and next page %d, want %d and %d",
			back.Root(), back.Meta().NextPage, p.ID(), p.ID()+1)
	}
	got, err := back.Read(back.Root())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got[page.HeaderSize:], []byte("durable")) {
		t.Errorf("the checkpointed root lost its contents in a power loss")
	}
}
