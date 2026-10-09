package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func TestEachKindOfChangeAloneMakesCloseCheckpoint(t *testing.T) {
	type setup struct {
		name   string
		change func(t *testing.T, s *Store, p page.Page)
		check  func(t *testing.T, back *Store, p page.Page)
	}
	cases := []setup{
		{"set root", func(t *testing.T, s *Store, p page.Page) {
			if err := s.SetRoot(p.ID()); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, back *Store, p page.Page) {
			if back.Root() != p.ID() {
				t.Errorf("Close dropped a root change: root is %d, want %d", back.Root(), p.ID())
			}
		}},
		{"write", func(t *testing.T, s *Store, p page.Page) {
			copy(p[page.HeaderSize:], "changed")
			if err := s.Write(p); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, back *Store, p page.Page) {
			got, err := back.Read(p.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(got[page.HeaderSize:], []byte("changed")) {
				t.Errorf("Close dropped a page write")
			}
		}},
		{"allocate", func(t *testing.T, s *Store, p page.Page) {
			mustAlloc(t, s)
		}, func(t *testing.T, back *Store, p page.Page) {
			if back.Meta().NextPage != p.ID()+2 {
				t.Errorf("Close dropped an allocation: next page is %d, want %d", back.Meta().NextPage, p.ID()+2)
			}
		}},
		{"free", func(t *testing.T, s *Store, p page.Page) {
			if err := s.Free(p.ID()); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, back *Store, p page.Page) {
			found := false
			for _, id := range back.FreePages() {
				found = found || id == p.ID()
			}
			if !found {
				t.Errorf("Close dropped a free: page %d is not free after reopening", p.ID())
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := tmpPath(t)
			s := mustCreate(t, path)
			p := mustAlloc(t, s)
			if err := s.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if s.Dirty() {
				t.Fatal("dirty right after a checkpoint")
			}
			c.change(t, s, p)
			if !s.Dirty() {
				t.Fatalf("%s did not mark the store dirty, so Close would skip the checkpoint", c.name)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			back := mustOpen(t, path)
			defer back.Close()
			c.check(t, back, p)
		})
	}
}

func TestStatsCountWhatActuallyHappened(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	for i := 0; i < ChunkPages-1; i++ {
		mustAlloc(t, s)
	}
	if err := s.Free(3); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(4); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	mustAlloc(t, s)
	got := s.Stats()
	want := Stats{Grown: 2, Allocated: ChunkPages - 1, Reused: 1, Freed: 2, Checkpoints: 2}
	if got != want {
		t.Fatalf("stats are %+v, want %+v", got, want)
	}
}

func TestACheckpointThatGrowsTheFileForItsFreeListRecordsTheNewSize(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	for int(s.Meta().NextPage) < ChunkPages {
		mustAlloc(t, s)
	}
	if err := s.Free(5); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if got := s.ListPages(); len(got) != 1 || got[0] != core.PageID(ChunkPages) {
		t.Fatalf("the free list should sit on page %d, the first page of a new chunk; it is on %v", ChunkPages, got)
	}
	if s.Meta().FilePages != 2*ChunkPages || s.Durable().FilePages != 2*ChunkPages {
		t.Fatalf("the checkpoint grew the file but records %d live and %d durable pages, want %d",
			s.Meta().FilePages, s.Durable().FilePages, 2*ChunkPages)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	back := mustOpen(t, path)
	defer back.Close()
	if err := back.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestThePageJustPastTheEndIsNotHandedOut(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	last := mustAlloc(t, s)
	past := s.Meta().NextPage
	if past != last.ID()+1 {
		t.Fatalf("next page is %d, want %d", past, last.ID()+1)
	}
	if _, err := s.Read(last.ID()); err != nil {
		t.Errorf("the last page handed out cannot be read: %v", err)
	}
	if _, err := s.Read(past); !errors.Is(err, ErrBadPageID) {
		t.Errorf("Read of page %d, one past the end, gave %v", past, err)
	}
	if err := s.Free(past); !errors.Is(err, ErrBadPageID) {
		t.Errorf("Free of page %d, one past the end, gave %v", past, err)
	}
	if err := s.SetRoot(past); !errors.Is(err, ErrBadPageID) {
		t.Errorf("SetRoot of page %d, one past the end, gave %v", past, err)
	}
	if err := s.Write(page.New(past, page.KindHeap)); !errors.Is(err, ErrBadPageID) {
		t.Errorf("Write of page %d, one past the end, gave %v", past, err)
	}
}

func TestEveryMethodRefusesAClosedStore(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	p := mustAlloc(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	calls := map[string]error{}
	calls["SetRoot"] = s.SetRoot(p.ID())
	_, calls["Read"] = s.Read(p.ID())
	calls["Write"] = s.Write(p)
	calls["Sync"] = s.Sync()
	calls["Verify"] = s.Verify()
	_, calls["Allocate"] = s.Allocate(page.KindHeap)
	calls["Free"] = s.Free(p.ID())
	calls["Checkpoint"] = s.Checkpoint()
	_, calls["DescribeMeta"] = s.DescribeMeta()
	for name, err := range calls {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s on a closed store gave %v, want ErrClosed", name, err)
		}
	}
}

func TestDiscardClosesWithoutCheckpointingAndCanBeRepeated(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	p := mustAlloc(t, s)
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(p.ID()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read straight after Discard gave %v, want ErrClosed", err)
	}
	if err := s.Discard(); err != nil {
		t.Errorf("a second Discard gave %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close after Discard gave %v, want nil", err)
	}
	back := mustOpen(t, path)
	defer back.Close()
	if back.Meta().NextPage != 1 {
		t.Errorf("Discard checkpointed: next page is %d after reopening, want 1", back.Meta().NextPage)
	}
}

func boundariesStore(t *testing.T) *Store {
	t.Helper()
	s := mustCreate(t, tmpPath(t))
	for i := 0; i < 10; i++ {
		mustAlloc(t, s)
	}
	if err := s.SetRoot(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []core.PageID{2, 3, 4} {
		if err := s.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(6); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("clean store: %v", err)
	}
	return s
}

func TestVerifyCatchesEachProblemThatOnlyOneOfItsChecksCanSee(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(*testing.T, *Store)
	}{
		{"the meta page on disk is valid but is not the last checkpoint", func(t *testing.T, s *Store) {
			newer := s.durable
			newer.Checkpoint++
			if err := s.pf.Write(encodeMeta(newer)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the live next page is behind the last checkpoint", func(t *testing.T, s *Store) {
			ahead := s.durable
			ahead.NextPage = s.nextPage + 1
			if err := s.pf.Write(encodeMeta(ahead)); err != nil {
				t.Fatal(err)
			}
			s.durable = ahead
		}},
		{"a free page has no status at all, so it could be used after being freed", func(t *testing.T, s *Store) {
			delete(s.status, s.free[0])
		}},
		{"a page past the end sits on the pending list", func(t *testing.T, s *Store) {
			s.pending = append(s.pending, 9999)
			s.status[9999] = stPending
		}},
		{"a free page is marked pending", func(t *testing.T, s *Store) {
			s.status[s.free[0]] = stPending
		}},
		{"a free-list page is marked free", func(t *testing.T, s *Store) {
			s.status[s.listPage[0]] = stFree
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := boundariesStore(t)
			defer s.Discard()
			c.corrupt(t, s)
			if err := s.Verify(); !errors.Is(err, ErrCorrupt) {
				t.Errorf("Verify gave %v where %s, want ErrCorrupt", err, c.name)
			}
		})
	}
}

func TestVerifyCatchesFreeListPagesInTheWrongOrder(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Discard()
	n := FreeListCapacity + 50
	for i := 0; i < n; i++ {
		mustAlloc(t, s)
	}
	for id := core.PageID(1); id <= core.PageID(n); id++ {
		if err := s.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if len(s.listPage) != 2 {
		t.Fatalf("expected two free-list pages, got %v", s.listPage)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	s.listPage[0], s.listPage[1] = s.listPage[1], s.listPage[0]
	if err := s.Verify(); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Verify accepted live free-list pages in a different order from the chain on disk: %v", err)
	}
}

func TestCreateTellsAStoreFromForeignDataOfAnySize(t *testing.T) {
	for _, size := range []int{5, 23, 24, 100, 4096} {
		path := tmpPath(t)
		junk := bytes.Repeat([]byte("x"), size)
		if err := os.WriteFile(path, junk, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Create(path)
		if err == nil || !strings.Contains(err.Error(), "not a scutedb store") {
			t.Errorf("Create over %d bytes of foreign data gave %v; it must not suggest Open", size, err)
		}
	}
	path := tmpPath(t)
	raw := make([]byte, 100)
	copy(raw[offMagic:], Magic[:])
	binary.BigEndian.PutUint32(raw[offVersion:], Version)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path); err == nil || !strings.Contains(err.Error(), "use Open") {
		t.Errorf("Create over a file that carries the magic gave %v; it should point at Open", err)
	}
}
