package bufpool

import (
	"errors"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

var errInjected = errors.New("injected failure")

type faultSource struct {
	inner      Source
	failRead   map[core.PageID]bool
	failWrite  map[core.PageID]bool
	failAlloc  bool
	failFree   bool
	reads      []core.PageID
	writes     []core.PageID
	readHook   func(core.PageID, page.Page) page.Page
	allocHook  func(page.Page) page.Page
	allocCalls int
}

func newFaultSource(inner Source) *faultSource {
	return &faultSource{inner: inner, failRead: map[core.PageID]bool{}, failWrite: map[core.PageID]bool{}}
}

func (s *faultSource) Read(id core.PageID) (page.Page, error) {
	if s.failRead[id] {
		return nil, errInjected
	}
	p, err := s.inner.Read(id)
	if err != nil {
		return nil, err
	}
	s.reads = append(s.reads, id)
	if s.readHook != nil {
		p = s.readHook(id, p)
	}
	return p, nil
}

func (s *faultSource) Write(p page.Page) error {
	if s.failWrite[p.ID()] {
		return errInjected
	}
	s.writes = append(s.writes, p.ID())
	return s.inner.Write(p)
}

func (s *faultSource) Allocate(kind page.Kind) (page.Page, error) {
	s.allocCalls++
	if s.failAlloc {
		return nil, errInjected
	}
	p, err := s.inner.Allocate(kind)
	if err != nil {
		return nil, err
	}
	if s.allocHook != nil {
		p = s.allocHook(p)
	}
	return p, nil
}

func (s *faultSource) Free(id core.PageID) error {
	if s.failFree {
		return errInjected
	}
	return s.inner.Free(id)
}

func seeded(t *testing.T, n int) (*MemSource, []core.PageID) {
	t.Helper()
	src := NewMemSource()
	ids := make([]core.PageID, n)
	for i := range ids {
		p, err := src.Allocate(page.KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		p[page.HeaderSize] = byte(i)
		if err := src.Write(p); err != nil {
			t.Fatal(err)
		}
		ids[i] = p.ID()
	}
	return src, ids
}

func mustPool(t *testing.T, src Source, opts Options) *Pool {
	t.Helper()
	p, err := New(src, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustFetch(t *testing.T, p *Pool, id core.PageID) *Frame {
	t.Helper()
	f, err := p.Fetch(id)
	if err != nil {
		t.Fatalf("Fetch(%d): %v", id, err)
	}
	return f
}

func mustUnpin(t *testing.T, p *Pool, f *Frame, dirty bool) {
	t.Helper()
	if err := p.Unpin(f, dirty); err != nil {
		t.Fatalf("Unpin(%d): %v", f.ID(), err)
	}
}

func sourceByte(t *testing.T, src Source, id core.PageID) byte {
	t.Helper()
	p, err := src.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	return p[page.HeaderSize]
}
