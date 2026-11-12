package bufpool

import (
	"errors"
	"fmt"
	"sync"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

var ErrNoSuchPage = errors.New("scutedb/bufpool: memory source has no such page")

type MemSource struct {
	mu    sync.Mutex
	pages map[core.PageID]page.Page
	free  []core.PageID
	next  core.PageID
}

func NewMemSource() *MemSource {
	return &MemSource{pages: map[core.PageID]page.Page{}, next: 1}
}

func (m *MemSource) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pages)
}

func (m *MemSource) Read(id core.PageID) (page.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[id]
	if !ok {
		return nil, fmt.Errorf("%w: read %d", ErrNoSuchPage, id)
	}
	return append(page.Page(nil), p...), nil
}

func (m *MemSource) Write(p page.Page) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(p) != page.Size {
		return fmt.Errorf("%w: page is %d bytes", ErrShortPage, len(p))
	}
	old, ok := m.pages[p.ID()]
	if !ok {
		return fmt.Errorf("%w: write %d", ErrNoSuchPage, p.ID())
	}
	copy(old, p)
	return nil
}

func (m *MemSource) Allocate(kind page.Kind) (page.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var id core.PageID
	if n := len(m.free); n > 0 {
		id = m.free[n-1]
		m.free = m.free[:n-1]
	} else {
		id = m.next
		m.next++
	}
	p := page.New(id, kind)
	m.pages[id] = p
	return append(page.Page(nil), p...), nil
}

func (m *MemSource) Free(id core.PageID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pages[id]; !ok {
		return fmt.Errorf("%w: free %d", ErrNoSuchPage, id)
	}
	delete(m.pages, id)
	m.free = append(m.free, id)
	return nil
}
