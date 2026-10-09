package pagestore

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

func layersMeta() Meta {
	return Meta{Version: Version, PageSize: page.Size, Root: 3, FreeListPage: 9, FreeCount: 2, NextPage: 10, FilePages: 16, Checkpoint: 4}
}

func TestCheckMetaRejectsEachInconsistencyOnItsOwn(t *testing.T) {
	if err := checkMeta(layersMeta(), 16); err != nil {
		t.Fatalf("a consistent meta was rejected: %v", err)
	}
	cases := []struct {
		name   string
		edit   func(*Meta)
		onDisk uint32
	}{
		{"next page is 0", func(m *Meta) { m.NextPage = 0 }, 16},
		{"more pages handed out than the file holds", func(m *Meta) { m.NextPage = 17 }, 16},
		{"the meta claims more pages than are on disk", func(m *Meta) {}, 15},
		{"the root is the next page to hand out", func(m *Meta) { m.Root = 10 }, 16},
		{"the root is past the end", func(m *Meta) { m.Root = 12 }, 16},
	}
	for _, c := range cases {
		m := layersMeta()
		c.edit(&m)
		if err := checkMeta(m, c.onDisk); !errors.Is(err, ErrCorrupt) {
			t.Errorf("checkMeta accepted a meta where %s: %v", c.name, err)
		}
	}
	m := layersMeta()
	m.Root = NoPage
	if err := checkMeta(m, 16); err != nil {
		t.Errorf("a store with no root was rejected: %v", err)
	}
	m.Root = 9
	if err := checkMeta(m, 16); err != nil {
		t.Errorf("a root on the last page handed out was rejected: %v", err)
	}
}

func TestCheckFreeSetRejectsEachInconsistencyOnItsOwn(t *testing.T) {
	m := layersMeta()
	if err := checkFreeSet(m, []core.PageID{4, 5}, []core.PageID{9}); err != nil {
		t.Fatalf("a consistent free set was rejected: %v", err)
	}
	cases := []struct {
		name    string
		entries []core.PageID
		lists   []core.PageID
	}{
		{"an entry is page 0", []core.PageID{0, 5}, []core.PageID{9}},
		{"an entry is the next page to hand out", []core.PageID{4, 10}, []core.PageID{9}},
		{"an entry is past the end", []core.PageID{4, 99}, []core.PageID{9}},
		{"an entry appears twice", []core.PageID{4, 4}, []core.PageID{9}},
		{"an entry is the root", []core.PageID{4, 3}, []core.PageID{9}},
		{"an entry is a free-list page", []core.PageID{4, 9}, []core.PageID{9}},
		{"the root is a free-list page", []core.PageID{4, 5}, []core.PageID{3}},
	}
	for _, c := range cases {
		if err := checkFreeSet(m, c.entries, c.lists); !errors.Is(err, ErrCorrupt) {
			t.Errorf("checkFreeSet accepted a free set where %s: %v", c.name, err)
		}
	}
	if err := checkFreeSet(m, []core.PageID{9}, []core.PageID{8}); err != nil {
		t.Errorf("the last page handed out may be free: %v", err)
	}
}

func TestReadFreeListRefusesAChainThatReachesThePageNotYetHandedOut(t *testing.T) {
	mf := newMemFile()
	pf := page.NewFile(mf)
	if err := pf.Grow(0, ChunkPages); err != nil {
		t.Fatal(err)
	}
	m := Meta{Version: Version, PageSize: page.Size, NextPage: 5, FilePages: ChunkPages, FreeListPage: 5, FreeCount: 1}
	lp := page.New(5, page.KindFreeList)
	binary.BigEndian.PutUint32(lp[FreeListHeader:], 2)
	lp.SetItemCount(1)
	if err := pf.Write(lp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFreeList(pf, m); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a well-formed free-list page at the next page to hand out was followed: %v", err)
	}
	m.NextPage = 6
	if _, _, err := readFreeList(pf, m); err != nil {
		t.Errorf("the same page is fine once it has been handed out: %v", err)
	}
}

func TestCreateInAMissingDirectoryFails(t *testing.T) {
	if _, err := Create(filepath.Join(t.TempDir(), "missing", "index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Create in a directory that does not exist gave %v, want os.ErrNotExist", err)
	}
}

type layersShortReader struct{ *memFile }

func (l layersShortReader) ReadAt(p []byte, off int64) (int, error) {
	n, _ := l.memFile.ReadAt(p[:len(p)/2], off)
	return n, nil
}

func TestABlankCheckThatReadsShortWithoutAnErrorStillFails(t *testing.T) {
	mf := newMemFile()
	if _, err := mf.WriteAt(make([]byte, 100), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openFile(layersShortReader{mf}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a read that came back short with no error gave %v, want io.ErrUnexpectedEOF", err)
	}
}

type layersCloseFails struct{ *memFile }

var errLayersClose = errors.New("layers: close failed")

func (l layersCloseFails) Close() error {
	l.memFile.Close()
	return errLayersClose
}

func TestCloseReportsAFailureToCloseTheFile(t *testing.T) {
	s, err := create(page.NewFile(layersCloseFails{newMemFile()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); !errors.Is(err, errLayersClose) {
		t.Errorf("Close swallowed the file's close error: %v", err)
	}
}

func TestVerifyReportsAFailedRead(t *testing.T) {
	mf := newMemFile()
	f := faultsWrap(mf)
	s, err := create(page.NewFile(f))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	f.arm(faultsRead, 1, nil, 0)
	if err := s.Verify(); err == nil {
		t.Error("Verify passed although reading the meta page failed")
	}
}
