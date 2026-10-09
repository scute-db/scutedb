package pagestore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/fileio"
	"github.com/scute-db/scutedb/internal/page"
)

func mustAllocOn(t *testing.T, s *Store, kind page.Kind) page.Page {
	t.Helper()
	p, err := s.Allocate(kind)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	return p
}

func mustReopen(t *testing.T, mf *memFile) *Store {
	t.Helper()
	s, _, err := openFile(mf)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	return s
}

func TestReopenMakesTheMetaItReadDurableBeforeReusingPages(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	old := mustAllocOn(t, s, page.KindHeap)
	copy(old[page.HeaderSize:], "committed")
	if err := s.Write(old); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(old.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	replacement := mustAllocOn(t, s, page.KindHeap)
	copy(replacement[page.HeaderSize:], "replacement")
	if err := s.Write(replacement); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(replacement.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Free(old.ID()); err != nil {
		t.Fatal(err)
	}

	mf.failAtSync = mf.syncs + 3
	if err := s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a checkpoint whose final fsync failed gave %v, want ErrPoisoned", err)
	}
	s.Close()
	mf.restart()

	back := mustReopen(t, mf)
	seen := back.Durable().Checkpoint
	for len(back.FreePages()) > 0 {
		p := mustAllocOn(t, back, page.KindHeap)
		copy(p[page.HeaderSize:], "overwrite")
		if err := back.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	back.Discard()
	mf.keepNothing()

	final := mustReopen(t, mf)
	defer final.Close()
	if final.Durable().Checkpoint != seen {
		t.Fatalf("the reopened store believed generation %d, but a power loss later recovered %d; "+
			"it handed out pages on the strength of a meta page that was never durable",
			seen, final.Durable().Checkpoint)
	}
	if err := final.Verify(); err != nil {
		t.Fatalf("Verify after the power loss: %v", err)
	}
	got, err := final.Read(final.Root())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got[page.HeaderSize:], []byte("replacement")) {
		t.Fatalf("the recovered root holds %q", got[page.HeaderSize:page.HeaderSize+11])
	}
}

func TestReopenAfterAFailedFinalSyncKeepsTheFreeListOpenable(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	a := mustAllocOn(t, s, page.KindHeap)
	mustAllocOn(t, s, page.KindHeap)
	if err := s.Free(a.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	mustAllocOn(t, s, page.KindHeap)
	mf.failAtSync = mf.syncs + 3
	if err := s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("want ErrPoisoned, got %v", err)
	}
	s.Close()
	mf.restart()

	back := mustReopen(t, mf)
	for len(back.FreePages()) > 0 {
		mustAllocOn(t, back, page.KindBTreeLeaf)
	}
	back.Discard()
	mf.keepNothing()

	final, _, err := openFile(mf)
	if err != nil {
		t.Fatalf("after a power loss the file no longer opens: %v", err)
	}
	defer final.Close()
	if err := final.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedSyncPoisonsTheStore(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	durable := s.Durable()

	p := mustAllocOn(t, s, page.KindHeap)
	copy(p[page.HeaderSize:], "never durable")
	if err := s.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}

	mf.failAtSync = mf.syncs + 1
	if err := s.Sync(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("a failed Sync gave %v, want ErrPoisoned", err)
	}
	mf.failAtSync = 0
	if err := s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("Checkpoint after a failed Sync gave %v; the page it would commit never reached the disk", err)
	}
	if err := s.Close(); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Close after a failed Sync gave %v, want ErrPoisoned", err)
	}

	mf.keepNothing()
	back := mustReopen(t, mf)
	defer back.Close()
	if back.Durable() != durable {
		t.Errorf("recovered %+v, want the last good checkpoint %+v", back.Durable(), durable)
	}
}

func TestACrashDuringCreateLeavesAFileThatStillOpens(t *testing.T) {
	keeps := map[string]func(int) bool{
		"nothing survives":    func(int) bool { return false },
		"everything survives": func(int) bool { return true },
		"odd writes survive":  func(i int) bool { return i%2 == 1 },
	}
	for failAt := 1; failAt <= 2; failAt++ {
		for name, keep := range keeps {
			for _, powerCut := range []bool{false, true} {
				mf := newMemFile()
				mf.failAtSync = failAt
				mf.failKeep = keep
				if _, err := create(page.NewFile(mf)); !errors.Is(err, ErrPoisoned) {
					t.Fatalf("sync %d, %s: create gave %v, want ErrPoisoned", failAt, name, err)
				}
				if powerCut {
					mf.powerLoss(keep)
				} else {
					mf.restart()
				}
				s, _, err := openFile(mf)
				if err != nil {
					t.Fatalf("sync %d failed during create, %s, power cut %v: the file cannot be opened: %v",
						failAt, name, powerCut, err)
				}
				if err := s.Verify(); err != nil {
					t.Fatalf("sync %d, %s, power cut %v: %v", failAt, name, powerCut, err)
				}
				s.Close()
			}
		}
	}
}

func TestAnAllZeroFileFromAnInterruptedCreateOpensAndCreates(t *testing.T) {
	for _, size := range []int{1, 100, page.Size, ChunkPages * page.Size} {
		path := tmpPath(t)
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open of %d zero bytes, what an interrupted create leaves: %v", size, err)
		}
		s.Close()

		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := Create(path)
		if err != nil {
			t.Fatalf("Create over %d zero bytes: %v", size, err)
		}
		c.Close()
	}
}

func TestAZeroFileLargerThanOneChunkIsStillForeign(t *testing.T) {
	path := tmpPath(t)
	if err := os.WriteFile(path, make([]byte, ChunkPages*page.Size+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrNotScuteDB) {
		t.Errorf("a create never grows past one chunk before its meta is durable, so a larger zero file "+
			"is not ours; Open gave %v, want ErrNotScuteDB", err)
	}
}

func TestOpenRejectsARootThatIsAFreeListPage(t *testing.T) {
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
	bad := s.Durable()
	bad.Root = s.ListPages()[0]
	if err := s.pf.Write(encodeMeta(bad)); err != nil {
		t.Fatal(err)
	}
	s.Discard()
	_, err := Open(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of a file whose root is its own free-list page gave %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "also a free-list page") {
		t.Errorf("the durable free list should be rejected on its own terms, before any live state is built; got %v", err)
	}
}

func TestVerifyComparesLiveStateWithTheDurableFreeList(t *testing.T) {
	build := func(t *testing.T) (*Store, []core.PageID) {
		s := mustCreate(t, tmpPath(t))
		var ids []core.PageID
		for i := 0; i < 6; i++ {
			ids = append(ids, mustAlloc(t, s).ID())
		}
		if err := s.SetRoot(ids[0]); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids[3:] {
			if err := s.Free(id); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := s.Verify(); err != nil {
			t.Fatalf("clean store: %v", err)
		}
		return s, ids
	}

	cases := []struct {
		name    string
		corrupt func(*Store, []core.PageID)
	}{
		{"a reusable page was in use at the last checkpoint", func(s *Store, ids []core.PageID) {
			s.free = append(s.free, ids[1])
			s.status[ids[1]] = stFree
		}},
		{"one list names a page twice while another page is on no list", func(s *Store, ids []core.PageID) {
			s.free = append(s.free[:0:0], s.free[0], s.free[0])
			s.status = map[core.PageID]pageState{s.free[0]: stFree, ids[1]: stFree, s.listPage[0]: stFreeList}
		}},
		{"one list names a page twice, which would hand it out twice", func(s *Store, ids []core.PageID) {
			s.free = append(s.free, s.free[0])
		}},
		{"a page is marked pending but is on no list", func(s *Store, ids []core.PageID) {
			s.status[ids[1]] = stPending
		}},
		{"the live free-list pages differ from the durable chain", func(s *Store, ids []core.PageID) {
			s.status[s.listPage[0]] = stFree
			s.free = append(s.free, s.listPage[0])
			s.listPage = nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ids := build(t)
			defer s.Discard()
			c.corrupt(s, ids)
			if err := s.Verify(); err == nil {
				t.Errorf("Verify accepted a store where %s", c.name)
			}
		})
	}
}

func TestPageIDsCannotWrapIntoTheMetaPage(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Discard()
	s.nextPage = core.PageID(MaxPages)
	s.filePages = MaxPages
	if _, err := s.Allocate(page.KindHeap); !errors.Is(err, ErrFull) {
		t.Fatalf("allocating past the last page id gave %v, want ErrFull", err)
	}
	if s.nextPage != core.PageID(MaxPages) {
		t.Fatalf("a refused allocation moved next page to %d", s.nextPage)
	}

	if got := growTo(10); got != 10+ChunkPages {
		t.Errorf("growTo(10) = %d, want %d", got, 10+ChunkPages)
	}
	if got := growTo(MaxPages - 3); got != MaxPages {
		t.Errorf("growTo near the limit = %d, want it capped at %d rather than wrapping", got, MaxPages)
	}
	if got := growTo(MaxPages); got != MaxPages {
		t.Errorf("growTo at the limit = %d, want %d", got, MaxPages)
	}
}

func TestAFreeListPageMustSayWhichPageItIs(t *testing.T) {
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
	at := page.Offset(s.ListPages()[0])
	s.Discard()
	binary.BigEndian.PutUint32(p[0:4], 999)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(p, at); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a free-list page whose header names another page was accepted: %v; "+
			"a misdirected write would go unnoticed", err)
	}
}

func TestAMetaPageThatNamesAnotherPageIsRejected(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(raw[0:4], 5)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Open of a page 0 that says it is page 5 gave %v, want ErrCorrupt", err)
	}
}

func TestGrowthDoesNotTrustTheFileSizeTheCacheReports(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	for int(s.Meta().NextPage) < ChunkPages {
		mustAllocOn(t, s, page.KindHeap)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	zeroFill := len(mf.log)
	edge := mustAllocOn(t, s, page.KindHeap)
	if edge.ID() != core.PageID(ChunkPages) || s.Meta().FilePages != 2*ChunkPages {
		t.Fatalf("expected page %d to grow the file to %d pages, got page %d and %d pages",
			ChunkPages, 2*ChunkPages, edge.ID(), s.Meta().FilePages)
	}
	mf.failAtSync = mf.syncs + 1
	mf.failKeep = func(i int) bool { return i != zeroFill }
	if err := s.Checkpoint(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("want ErrPoisoned, got %v", err)
	}
	s.Close()
	mf.restart()

	back := mustReopen(t, mf)
	if want := uint32(ChunkPages); back.Meta().FilePages != want {
		t.Fatalf("reopened with %d pages, want the checkpointed %d", back.Meta().FilePages, want)
	}
	mustAllocOn(t, back, page.KindHeap)
	if err := back.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	back.Discard()
	mf.keepNothing()

	final, _, err := openFile(mf)
	if err != nil {
		t.Fatalf("the page cache said the file was already big enough, so the growth was never rewritten: %v", err)
	}
	defer final.Close()
	if err := final.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAlwaysMakesTheDirectoryEntryDurable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/index.db"

	f, err := fileio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := create(page.NewFile(f))
	if err != nil {
		t.Fatal(err)
	}
	s.Discard()

	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := fileio.SyncDir(dir); err == nil {
		t.Skip("this environment can fsync a directory it cannot read, so the detector does not work here")
	}

	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open succeeded without fsyncing the directory; a file left by an interrupted Create " +
			"may have a name that exists only in the cache, and a power loss would delete it")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open(path) failed, but not because the directory could not be fsynced: %v", err)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	back, err := Open(path)
	if err != nil {
		t.Fatalf("once the directory can be fsynced, Open should recover the file: %v", err)
	}
	defer back.Close()
	if err := back.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRunningOutOfPageIDsInACheckpointDoesNotPoison(t *testing.T) {
	s := mustCreate(t, tmpPath(t))
	defer s.Discard()
	a := mustAlloc(t, s)
	mustAlloc(t, s)
	if err := s.Free(a.ID()); err != nil {
		t.Fatal(err)
	}

	realNext, realFile := s.nextPage, s.filePages
	s.nextPage, s.filePages = core.PageID(MaxPages), MaxPages
	err := s.Checkpoint()
	if !errors.Is(err, ErrFull) {
		t.Fatalf("a checkpoint that cannot get a free-list page gave %v, want ErrFull", err)
	}
	if errors.Is(err, ErrPoisoned) {
		t.Fatalf("ErrFull happens before anything durable is touched, so it must not poison the store: %v", err)
	}

	s.nextPage, s.filePages = realNext, realFile
	if err := s.Checkpoint(); err != nil {
		t.Fatalf("after a refused checkpoint the store should still work: %v", err)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestAPoisonedErrorKeepsItsCause(t *testing.T) {
	mf := newMemFile()
	s, err := create(page.NewFile(mf))
	if err != nil {
		t.Fatal(err)
	}
	mustAllocOn(t, s, page.KindHeap)
	mf.failAtSync = mf.syncs + 1
	err = s.Checkpoint()
	if !errors.Is(err, ErrPoisoned) || !errors.Is(err, errSyncFailed) {
		t.Fatalf("got %v; want both ErrPoisoned and the underlying fsync error in the chain", err)
	}
	if _, err := s.Allocate(page.KindHeap); !errors.Is(err, errSyncFailed) {
		t.Errorf("later calls should still name the original cause, got %v", err)
	}
}

func TestANewerFormatIsCalledNewerNotForeign(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	magic := append([]byte(nil), raw[16:24]...)
	binary.BigEndian.PutUint32(raw[offVersion:], Version+1)
	raw[4] = 0x7F
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[16:24], magic) {
		t.Fatal("changing the version changed the magic; the magic must never carry the version")
	}
	if _, err := Open(path); !errors.Is(err, ErrBadVersion) {
		t.Errorf("a version-%d file whose header layout also changed gave %v; want ErrBadVersion, "+
			"because identity and version must be checked before anything a new version may change", Version+1, err)
	}
}

func interruptedCreate(t *testing.T, path string) {
	t.Helper()
	f, err := fileio.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := create(page.NewFile(f))
	if err != nil {
		t.Fatal(err)
	}
	s.Discard()
}

func unreadable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := fileio.SyncDir(dir); err == nil {
		t.Skip("this environment can fsync a directory it cannot read, so the detector does not work here")
	}
}

func TestOpenThroughASymlinkFsyncsTheDirectoryHoldingTheFile(t *testing.T) {
	linkDir, realDir := t.TempDir(), t.TempDir()
	realPath := filepath.Join(realDir, "real.db")
	linkPath := filepath.Join(linkDir, "index.db")
	interruptedCreate(t, realPath)
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	unreadable(t, realDir)

	if s, err := Open(linkPath); err == nil {
		s.Close()
		t.Fatalf("Open through a symlink succeeded without fsyncing %s, which holds the file's real entry", realDir)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open(linkPath) failed, but not because the directory could not be fsynced: %v", err)
	}
}

func TestCreateThroughADanglingSymlinkFsyncsTheDirectoryHoldingTheFile(t *testing.T) {
	linkDir, realDir := t.TempDir(), t.TempDir()
	realPath := filepath.Join(realDir, "real.db")
	linkPath := filepath.Join(linkDir, "index.db")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	unreadable(t, realDir)

	if s, err := Create(linkPath); err == nil {
		s.Close()
		t.Fatalf("Create through a dangling symlink made %s without fsyncing its directory", realPath)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Create(linkPath) failed, but not because the directory could not be fsynced: %v", err)
	}
}

func TestADotDotAfterASymlinkedDirectoryFsyncsTheRealParent(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "realparent")
	linkDir := filepath.Join(base, "linkdir")
	for _, d := range []string{filepath.Join(realParent, "target"), linkDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(realParent, "target"), filepath.Join(linkDir, "link")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkDir, "link") + "/../index.db"
	interruptedCreate(t, filepath.Join(realParent, "index.db"))
	unreadable(t, realParent)

	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatalf("Open(%s) succeeded; the kernel resolves it into %s, which was never fsynced", path, realParent)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open(path) failed, but not because the directory could not be fsynced: %v", err)
	}
}

func TestSymlinkedPathsStillOpenWhenEverythingIsFine(t *testing.T) {
	linkDir, realDir := t.TempDir(), t.TempDir()
	realPath := filepath.Join(realDir, "real.db")
	linkPath := filepath.Join(linkDir, "index.db")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	s, err := Create(linkPath)
	if err != nil {
		t.Fatalf("Create through a dangling symlink: %v", err)
	}
	p := mustAlloc(t, s)
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := Open(linkPath)
	if err != nil {
		t.Fatalf("Open through the symlink: %v", err)
	}
	defer back.Close()
	if back.Root() != p.ID() {
		t.Errorf("root is %d through the symlink, want %d", back.Root(), p.ID())
	}
}

func TestAMissingDatabaseIsAnErrorNotAnEmptyDatabase(t *testing.T) {
	path := tmpPath(t)
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open of a path that does not exist returned a store; a lost directory entry would " +
			"then look like an empty database instead of a missing one")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open of a missing path gave %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open created the file it was asked to open: %v", err)
	}
}

func TestATruncatedStoreIsCorruptNotForeign(t *testing.T) {
	path := tmpPath(t)
	s := mustCreate(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a store cut down to 100 bytes gave %v; it is recognisably ours, so want ErrCorrupt", err)
	}
}

func TestCreateSaysWhatItFoundInTheWay(t *testing.T) {
	ours := tmpPath(t)
	s := mustCreate(t, ours)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ours); err == nil || !strings.Contains(err.Error(), "use Open") {
		t.Errorf("Create over an existing store gave %v; it should point the caller at Open", err)
	}

	foreign := tmpPath(t)
	if err := os.WriteFile(foreign, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(foreign); err == nil || !strings.Contains(err.Error(), "not a scutedb store") {
		t.Errorf("Create over a foreign file gave %v; it should not suggest Open", err)
	}
}

func TestAResolvedPathLongerThanPathMaxStillOpens(t *testing.T) {
	long := func(c byte) string { return strings.Repeat(string(c), 250) }
	shallow := filepath.Join(t.TempDir(), long('a'), long('b'), long('c'))
	if err := os.MkdirAll(shallow, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(shallow); err != nil {
		t.Fatal(err)
	}
	err = os.MkdirAll(filepath.Join(long('d'), long('e')), 0o700)
	if err == nil {
		err = os.Symlink(filepath.Join(long('d'), long('e')), "S")
	}
	if cerr := os.Chdir(home); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(shallow, "S", "index.db")
	if len(path) >= 1024 {
		t.Skipf("the temp directory is too deep for this layout: %d bytes", len(path))
	}

	s, err := Create(path)
	if err != nil {
		t.Fatalf("Create of a %d-byte path the kernel can reach: %v", len(path), err)
	}
	p := mustAlloc(t, s)
	if err := s.SetRoot(p.ID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := Open(path)
	if err != nil {
		t.Fatalf("a valid store whose resolved path is longer than PATH_MAX cannot be reopened: %v", err)
	}
	defer back.Close()
	if back.Root() != p.ID() {
		t.Errorf("root is %d, want %d", back.Root(), p.ID())
	}
}

func TestADotDotThatLeadsWhereTheTextDoesNotStillOpens(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"deep/dir", "deep/c", "linkdir"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "deep", "dir"), filepath.Join(base, "linkdir", "link")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "linkdir", "link") + "/../c/index.db"

	s, err := Create(path)
	if err != nil {
		t.Fatalf("Create(%s): the kernel resolves it into deep/c, but got %v", path, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	back.Close()
	if _, err := os.Stat(filepath.Join(base, "deep", "c", "index.db")); err != nil {
		t.Errorf("the store is not where the kernel put it: %v", err)
	}
}

func TestAChainOfSymlinksFsyncsTheDirectoryAtTheEnd(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	real := filepath.Join(c, "real.db")
	interruptedCreate(t, real)
	middle := filepath.Join(b, "middle")
	first := filepath.Join(a, "first")
	if err := os.Symlink(real, middle); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(middle, first); err != nil {
		t.Fatal(err)
	}
	unreadable(t, c)
	if s, err := Open(first); err == nil {
		s.Close()
		t.Fatalf("Open through two symlinks succeeded without fsyncing %s, which holds the file", c)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open(first) failed, but not because the directory could not be fsynced: %v", err)
	}
}

func TestARelativeSymlinkResolvesFromItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	interruptedCreate(t, filepath.Join(sub, "real.db"))
	link := filepath.Join(dir, "index.db")
	if err := os.Symlink("sub/real.db", link); err != nil {
		t.Fatal(err)
	}
	unreadable(t, sub)
	if s, err := Open(link); err == nil {
		s.Close()
		t.Fatalf("Open through a relative symlink succeeded without fsyncing %s", sub)
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open(link) failed, but not because the directory could not be fsynced: %v", err)
	}
}
