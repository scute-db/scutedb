package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "index.db")
}

func mustCreate(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustAlloc(t *testing.T, s *Store) page.Page {
	t.Helper()
	p, err := s.Allocate(page.KindBTreeLeaf)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	return p
}

func TestCreateWritesAReadableMetaPage(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	m := s.Meta()
	if m.Version != Version || m.PageSize != page.Size {
		t.Fatalf("meta says version %d page size %d", m.Version, m.PageSize)
	}
	if m.Root != NoPage || m.FreeListPage != NoPage || m.FreeCount != 0 {
		t.Fatalf("a fresh store should have no root and no free pages: %+v", m)
	}
	if m.NextPage != 1 {
		t.Fatalf("next page is %d, want 1 — page 0 is the meta page", m.NextPage)
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("Verify on a fresh store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(ChunkPages) * page.Size; info.Size() != want {
		t.Errorf("a fresh file is %d bytes, want one chunk of %d", info.Size(), want)
	}
}

func TestGoldenMetaLayout(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		at    int
		bytes []byte
		what  string
	}{
		{0, []byte{0, 0, 0, 0}, "page id 0"},
		{4, []byte{byte(page.KindMeta)}, "kind meta"},
		{8, []byte{0, 0x38}, "free start 56 = 16 header + 40 meta"},
		{10, []byte{0x10, 0x00}, "free end 4096"},
		{16, []byte{'S', 'C', 'U', 'T', 'E', 'D', 'B', 0x00}, "magic, with no version inside it"},
		{24, []byte{0, 0, 0, 1}, "format version 1"},
		{28, []byte{0, 0, 0x10, 0x00}, "page size 4096"},
		{32, []byte{0, 0, 0, 0}, "root, none yet"},
		{36, []byte{0, 0, 0, 0}, "free list page, none"},
		{40, []byte{0, 0, 0, 0}, "free count 0"},
		{44, []byte{0, 0, 0, 1}, "next page 1"},
		{48, []byte{0, 0, 0, 16}, "file holds 16 pages"},
		{52, []byte{0, 0, 0, 1}, "checkpoint generation 1"},
	}
	for _, w := range want {
		got := raw[w.at : w.at+len(w.bytes)]
		if !bytes.Equal(got, w.bytes) {
			t.Errorf("bytes %d..%d are % 02X, want % 02X (%s)",
				w.at, w.at+len(w.bytes)-1, got, w.bytes, w.what)
		}
	}
}

func TestOpenRejectsAForeignFile(t *testing.T) {
	path := tmpPath(t)
	junk := bytes.Repeat([]byte("this is not a database, it is a poem.\n"), 200)
	if err := os.WriteFile(path, junk, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrNotScuteDB) {
		t.Fatalf("Open of a text file gave %v, want ErrNotScuteDB", err)
	}
}

func TestOpenRejectsAShortFile(t *testing.T) {
	path := tmpPath(t)
	if err := os.WriteFile(path, []byte("SCUTEDB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrNotScuteDB) {
		t.Fatalf("Open of a 7-byte file gave %v, want ErrNotScuteDB", err)
	}
}

func TestOpenRejectsAVersionItCannotRead(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(raw[offVersion:], Version+1)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("Open of a future version gave %v, want ErrBadVersion", err)
	}
}

func TestOpenRejectsADifferentPageSize(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(raw[offPageSize:], 8192)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrBadPageSize) {
		t.Fatalf("Open of an 8192-byte-page file gave %v, want ErrBadPageSize", err)
	}
}

func TestAllocateHandsOutConsecutivePages(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	for want := core.PageID(1); want <= 10; want++ {
		p := mustAlloc(t, s)
		if p.ID() != want {
			t.Fatalf("allocation %d handed out page %d", want, p.ID())
		}
		if p.Kind() != page.KindBTreeLeaf {
			t.Fatalf("page %d has kind %s, want btree-leaf", p.ID(), p.Kind())
		}
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestAFreedPageIsReusableOnlyAfterACheckpoint(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()

	a := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(a.ID()); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got := s.PendingPages(); len(got) != 1 || got[0] != a.ID() {
		t.Fatalf("pending is %v, want [%d]", got, a.ID())
	}
	if got := s.FreePages(); len(got) != 0 {
		t.Fatalf("free is %v before a checkpoint, want empty", got)
	}

	early := mustAlloc(t, s)
	if early.ID() == a.ID() {
		t.Fatalf("page %d was handed out again before the checkpoint that freed it", a.ID())
	}

	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if len(s.PendingPages()) != 0 {
		t.Fatalf("pending is %v after a checkpoint, want empty", s.PendingPages())
	}
	again := mustAlloc(t, s)
	if again.ID() != a.ID() {
		t.Errorf("after the checkpoint, allocate handed out page %d, want the freed page %d", again.ID(), a.ID())
	}
	if st := s.Stats(); st.Reused != 1 {
		t.Errorf("stats say %d reused, want 1", st.Reused)
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestFreeListHandsBackTheMostRecentlyFreed(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()

	var ids []core.PageID
	for i := 0; i < 5; i++ {
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
	for i := range ids {
		got := mustAlloc(t, s).ID()
		if want := ids[len(ids)-1-i]; got != want {
			t.Fatalf("reuse %d handed out page %d, want %d — the most recently freed page comes back first", i, got, want)
		}
	}
}

func TestDoubleFreeIsRejected(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	p := mustAlloc(t, s)
	if err := s.Free(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(p.ID()); !errors.Is(err, ErrDoubleFree) {
		t.Errorf("freeing a pending page again gave %v, want ErrDoubleFree", err)
	}
	if len(s.PendingPages()) != 1 {
		t.Errorf("pending is %v after a rejected double free, want one page", s.PendingPages())
	}

	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(p.ID()); !errors.Is(err, ErrDoubleFree) {
		t.Errorf("freeing a page that is already on the free list gave %v, want ErrDoubleFree", err)
	}
}

func TestFreeingTheMetaPageIsRejected(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	if err := s.Free(MetaPage); !errors.Is(err, ErrFreeMeta) {
		t.Errorf("freeing page 0 gave %v, want ErrFreeMeta", err)
	}
}

func TestFreeingAPageNeverHandedOutIsRejected(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Close()
	mustAlloc(t, s)
	if err := s.Free(9); !errors.Is(err, ErrBadPageID) {
		t.Errorf("freeing an unallocated page gave %v, want ErrBadPageID", err)
	}
	if _, err := s.Read(9); !errors.Is(err, ErrBadPageID) {
		t.Errorf("reading an unallocated page gave %v, want ErrBadPageID", err)
	}
}

func TestFileGrowsOneChunkAtATime(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	defer s.Close()

	sizeNow := func() int64 {
		t.Helper()
		if err := s.Sync(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}

	chunk := int64(ChunkPages) * page.Size
	if got := sizeNow(); got != chunk {
		t.Fatalf("fresh file is %d bytes, want %d", got, chunk)
	}

	for i := 0; i < ChunkPages-1; i++ {
		mustAlloc(t, s)
	}
	if got := sizeNow(); got != chunk {
		t.Errorf("after filling the first chunk the file is %d bytes, want %d — no growth yet", got, chunk)
	}
	if s.Stats().Grown != 1 {
		t.Errorf("the file grew %d times filling the first chunk, want 1 (the create)", s.Stats().Grown)
	}

	mustAlloc(t, s)
	if got, want := sizeNow(), 2*chunk; got != want {
		t.Errorf("after one more page the file is %d bytes, want %d", got, want)
	}
	if s.Stats().Grown != 2 {
		t.Errorf("the file grew %d times, want 2", s.Stats().Grown)
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestRootSurvivesARestart(t *testing.T) {
	path := tmpPath(t)

	s := mustCreate(t, path)
	root := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.SetRoot(root.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if back.Root() != root.ID() {
		t.Fatalf("after a restart the root is page %d, want %d", back.Root(), root.ID())
	}
	if err := back.Verify(); err != nil {
		t.Fatalf("Verify after a restart: %v", err)
	}
	if back.Meta().NextPage != 3 {
		t.Errorf("after a restart the next page is %d, want 3 — the high-water mark must persist",
			back.Meta().NextPage)
	}
	next := mustAlloc(t, back)
	if next.ID() != 3 {
		t.Errorf("the first allocation after a restart handed out page %d, want 3", next.ID())
	}
}

func TestWorkWithoutACheckpointIsLostOnRestart(t *testing.T) {
	path := tmpPath(t)

	s := mustCreate(t, path)
	first := mustAlloc(t, s)
	if err := s.SetRoot(first.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	second := mustAlloc(t, s)
	if err := s.SetRoot(second.ID()); err != nil {
		t.Fatal(err)
	}
	if !s.Dirty() {
		t.Fatal("the store should be dirty after moving the root")
	}
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if back.Root() != first.ID() {
		t.Errorf("after a crash the root is page %d, want the last checkpointed root %d",
			back.Root(), first.ID())
	}
	if err := back.Verify(); err != nil {
		t.Errorf("a file abandoned without a checkpoint should still be structurally valid: %v", err)
	}
	if back.Meta().Checkpoint != 2 {
		t.Errorf("checkpoint generation is %d, want 2 — create plus one explicit checkpoint",
			back.Meta().Checkpoint)
	}
}

func TestFreeListSurvivesARestart(t *testing.T) {
	path := tmpPath(t)

	s := mustCreate(t, path)
	var ids []core.PageID
	for i := 0; i < 6; i++ {
		ids = append(ids, mustAlloc(t, s).ID())
	}
	for _, id := range ids[:4] {
		if err := s.Free(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	wantFree := s.FreePages()
	wantLists := s.ListPages()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if err := back.Verify(); err != nil {
		t.Fatalf("Verify after a restart: %v", err)
	}
	gotFree, gotLists := back.FreePages(), back.ListPages()
	if len(gotFree) != len(wantFree) || len(gotLists) != len(wantLists) {
		t.Fatalf("after a restart free is %v and list pages %v, were %v and %v",
			gotFree, gotLists, wantFree, wantLists)
	}
	for i := range wantFree {
		if gotFree[i] != wantFree[i] {
			t.Fatalf("after a restart free is %v, was %v", gotFree, wantFree)
		}
	}
	reused := mustAlloc(t, back).ID()
	if want := wantFree[len(wantFree)-1]; reused != want {
		t.Errorf("after a restart the first allocation took page %d, want %d", reused, want)
	}
}

func TestCloseCheckpointsPendingWork(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	p := mustAlloc(t, s)
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}
	if !s.Dirty() {
		t.Fatal("expected the store to be dirty")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	if back.Root() != p.ID() {
		t.Errorf("Close did not checkpoint: root is %d, want %d", back.Root(), p.ID())
	}
}

func TestOperationsAfterCloseAreRejected(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Allocate(page.KindHeap); !errors.Is(err, ErrClosed) {
		t.Errorf("Allocate after Close gave %v, want ErrClosed", err)
	}
	if err := s.Free(1); !errors.Is(err, ErrClosed) {
		t.Errorf("Free after Close gave %v, want ErrClosed", err)
	}
	if err := s.Checkpoint(); !errors.Is(err, ErrClosed) {
		t.Errorf("Checkpoint after Close gave %v, want ErrClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("a second Close gave %v, want nil", err)
	}
}

func TestOpenCreatesAnEmptyFile(t *testing.T) {
	path := tmpPath(t)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open of a zero-byte file should create: %v", err)
	}
	defer s.Close()
	if s.Meta().NextPage != 1 {
		t.Errorf("next page is %d, want 1", s.Meta().NextPage)
	}
}

func TestCreateRefusesAnExistingFile(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path); err == nil {
		t.Error("Create on a file that already holds a database should fail")
	}
}

func freeListPage(t *testing.T, s *Store) page.Page {
	t.Helper()
	if len(s.listPage) == 0 {
		t.Fatal("expected at least one free-list page")
	}
	p, err := s.pf.Read(s.listPage[0])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func rewrite(t *testing.T, s *Store, p page.Page) {
	t.Helper()
	if err := s.pf.Write(p); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCatchesCorruption(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(*testing.T, *Store)
	}{
		{"the meta page on disk no longer matches the last checkpoint", func(t *testing.T, s *Store) {
			raw, err := s.pf.Read(MetaPage)
			if err != nil {
				t.Fatal(err)
			}
			binary.BigEndian.PutUint32(raw[offRoot:], 900)
			rewrite(t, s, raw)
		}},
		{"a free-list page has the wrong kind", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			p.SetKind(page.KindHeap)
			rewrite(t, s, p)
		}},
		{"the free-list chain loops", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			binary.BigEndian.PutUint32(p[offFreeListNext:], uint32(p.ID()))
			rewrite(t, s, p)
		}},
		{"the free-list chain leaves the file", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			binary.BigEndian.PutUint32(p[offFreeListNext:], 4000)
			rewrite(t, s, p)
		}},
		{"a free-list page claims more entries than fit", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			p.SetItemCount(FreeListCapacity + 1)
			rewrite(t, s, p)
		}},
		{"the free list names one page twice", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			copy(p[FreeListHeader+4:FreeListHeader+8], p[FreeListHeader:FreeListHeader+4])
			rewrite(t, s, p)
		}},
		{"the free list names a page never handed out", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			binary.BigEndian.PutUint32(p[FreeListHeader:], 9999)
			rewrite(t, s, p)
		}},
		{"the free list names the meta page", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			binary.BigEndian.PutUint32(p[FreeListHeader:], uint32(MetaPage))
			rewrite(t, s, p)
		}},
		{"the free list names the durable root", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			binary.BigEndian.PutUint32(p[FreeListHeader:], uint32(s.durable.Root))
			rewrite(t, s, p)
		}},
		{"the free list names its own page", func(t *testing.T, s *Store) {
			p := freeListPage(t, s)
			binary.BigEndian.PutUint32(p[FreeListHeader:], uint32(p.ID()))
			rewrite(t, s, p)
		}},
		{"the live root was never handed out", func(t *testing.T, s *Store) { s.root = 900 }},
		{"the live root is free", func(t *testing.T, s *Store) { s.root = s.free[0] }},
		{"more pages handed out than the file holds", func(t *testing.T, s *Store) { s.nextPage = 9999 }},
		{"the next page went backwards", func(t *testing.T, s *Store) { s.nextPage = 1 }},
		{"a page is both free and pending", func(t *testing.T, s *Store) { s.pending = append(s.pending, s.free[0]) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustCreate(t, tmpPath(t))
			defer s.Discard()
			var ids []core.PageID
			for i := 0; i < 6; i++ {
				ids = append(ids, mustAlloc(t, s).ID())
			}
			if err := s.SetRoot(ids[0]); err != nil {
				t.Fatal(err)
			}
			for _, id := range []core.PageID{ids[2], ids[3], ids[4]} {
				if err := s.Free(id); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Checkpoint(); err != nil {
				t.Fatal(err)
			}
			if err := s.Verify(); err != nil {
				t.Fatalf("the clean store did not verify: %v", err)
			}
			c.corrupt(t, s)
			if err := s.Verify(); err == nil {
				t.Errorf("Verify accepted a store where %s", c.name)
			}
		})
	}
}

func TestOpenRejectsACorruptFreeList(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	a := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(a.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	p := freeListPage(t, s)
	binary.BigEndian.PutUint32(p[offFreeListNext:], uint32(p.ID()))
	rewrite(t, s, p)
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open of a file whose free list loops gave %v, want ErrCorrupt", err)
	}
}

func TestCorruptedMetaOnDiskIsRejectedOnOpen(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[4] = byte(page.KindHeap)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open of a file whose page 0 is not a meta page gave %v, want ErrCorrupt", err)
	}
}

func TestDescribeMetaDecodesWhatIsOnDisk(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	defer s.Close()
	root := mustAlloc(t, s)
	if err := s.SetRoot(root.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	out, err := s.DescribeMeta()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"53 43 55 54 45 44 42 00", "format version = 1", "page size = 4096", "root = page 1",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("DescribeMeta is missing %q\n%s", want, out)
		}
	}
}

func TestPagesWrittenBeforeACheckpointAreStillReadable(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)

	p := mustAlloc(t, s)
	copy(p[page.HeaderSize:], []byte("payload"))
	if err := s.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	back := mustOpen(t, path)
	defer back.Close()
	got, err := back.Read(back.Root())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got[page.HeaderSize:], []byte("payload")) {
		t.Errorf("page contents did not survive the restart: % 02X", got[page.HeaderSize:page.HeaderSize+8])
	}
}

func TestPageZeroIsASafeSentinel(t *testing.T) {
	if NoPage != MetaPage {
		t.Fatalf("NoPage is %d and MetaPage is %d; the format relies on them being the same page", NoPage, MetaPage)
	}

	s := mustCreate(t, tmpPath(t))
	defer s.Close()

	if err := s.SetRoot(NoPage); err != nil {
		t.Fatalf("SetRoot(NoPage) should mean no root: %v", err)
	}
	if err := s.Verify(); err != nil {
		t.Errorf("a store with no root should verify: %v", err)
	}
	if err := s.Free(MetaPage); !errors.Is(err, ErrFreeMeta) {
		t.Errorf("page 0 must never become free, or 0 could not mean end-of-list: %v", err)
	}

	p := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	lp := freeListPage(t, s)
	if next := binary.BigEndian.Uint32(lp[offFreeListNext:]); next != uint32(NoPage) {
		t.Errorf("the last free-list page points at %d, want %d for end of chain", next, NoPage)
	}
}
