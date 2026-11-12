package bufpool

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

var (
	ErrMTRDone  = errors.New("scutedb/bufpool: mini-transaction already committed or aborted")
	ErrNotHeld  = errors.New("scutedb/bufpool: frame is not held by this mini-transaction")
	ErrModified = errors.New("scutedb/bufpool: page was changed in this mini-transaction")
	ErrUpgrade  = errors.New("scutedb/bufpool: a read latch cannot be upgraded to a write latch")
	ErrUndoSet  = errors.New("scutedb/bufpool: this mini-transaction already has an undo record")
)

type Undo struct {
	Txn     uint64
	PrevLSN uint64
	Payload []byte
}

type latchMode uint8

const (
	pinOnly latchMode = iota
	shared
	exclusive
)

type hold struct {
	f        *Frame
	mode     latchMode
	before   page.Page
	verified bool
}

type MTR struct {
	pool    *Pool
	holds   []*hold
	byID    map[core.PageID]*hold
	allocs  []core.PageID
	frees   []core.PageID
	undo    Undo
	hasUndo bool
	done    bool
}

func (p *Pool) Begin() *MTR { return &MTR{pool: p, byID: map[core.PageID]*hold{}} }

func (m *MTR) Pool() *Pool { return m.pool }

func (m *MTR) usable() error {
	if m.done {
		return ErrMTRDone
	}
	return nil
}

func (m *MTR) Holds(id core.PageID) bool {
	_, ok := m.byID[id]
	return ok
}

func (m *MTR) Held() int { return len(m.holds) }

func (m *MTR) keep(f *Frame) {
	h := &hold{f: f}
	m.holds = append(m.holds, h)
	m.byID[f.ID()] = h
}

func (m *MTR) Fetch(id core.PageID) (*Frame, error) {
	if err := m.usable(); err != nil {
		return nil, err
	}
	if h, ok := m.byID[id]; ok {
		return h.f, nil
	}
	f, err := m.pool.Fetch(id)
	if err != nil {
		return nil, err
	}
	m.keep(f)
	return f, nil
}

func (m *MTR) NewPage(kind page.Kind) (*Frame, error) {
	if err := m.usable(); err != nil {
		return nil, err
	}
	f, err := m.pool.NewPage(kind)
	if err != nil {
		return nil, err
	}
	m.allocs = append(m.allocs, f.ID())
	m.keep(f)
	return f, nil
}

func (m *MTR) Alloc(kind page.Kind) (core.PageID, error) {
	if err := m.usable(); err != nil {
		return 0, err
	}
	f, err := m.pool.NewPage(kind)
	if err != nil {
		return 0, err
	}
	id := f.ID()
	m.allocs = append(m.allocs, id)
	return id, m.pool.Unpin(f, false)
}

func (m *MTR) Free(id core.PageID) error {
	if err := m.usable(); err != nil {
		return err
	}
	m.frees = append(m.frees, id)
	return nil
}

func (m *MTR) held(f *Frame) (*hold, error) {
	if err := m.usable(); err != nil {
		return nil, err
	}
	if f == nil {
		return nil, ErrNotHeld
	}
	h, ok := m.byID[f.ID()]
	if !ok || h.f != f {
		return nil, fmt.Errorf("%w: page %d", ErrNotHeld, f.ID())
	}
	return h, nil
}

func (m *MTR) WLatch(f *Frame) error {
	h, err := m.held(f)
	if err != nil {
		return err
	}
	switch h.mode {
	case exclusive:
		return nil
	case shared:
		return fmt.Errorf("%w: page %d", ErrUpgrade, f.ID())
	}
	m.pool.WLatch(f)
	h.before = append(page.Page(nil), f.buf...)
	h.verified = f.Verified()
	h.mode = exclusive
	return nil
}

func (m *MTR) RLatch(f *Frame) error {
	h, err := m.held(f)
	if err != nil {
		return err
	}
	if h.mode == pinOnly {
		m.pool.RLatch(f)
		h.mode = shared
	}
	return nil
}

func (h *hold) changed() bool { return h.mode == exclusive && !bytes.Equal(h.before, h.f.buf) }

func (m *MTR) Release(f *Frame) error {
	h, err := m.held(f)
	if err != nil {
		return err
	}
	if h.changed() {
		return fmt.Errorf("%w: page %d cannot be released before Commit", ErrModified, f.ID())
	}
	m.unlatch(h)
	delete(m.byID, f.ID())
	for i, x := range m.holds {
		if x == h {
			m.holds = append(m.holds[:i], m.holds[i+1:]...)
			break
		}
	}
	return m.pool.Unpin(f, false)
}

func (m *MTR) unlatch(h *hold) {
	switch h.mode {
	case exclusive:
		m.pool.WUnlatch(h.f)
	case shared:
		m.pool.RUnlatch(h.f)
	}
	h.mode = pinOnly
}

func (m *MTR) Modified() []core.PageID {
	var out []core.PageID
	for _, h := range m.holds {
		if h.changed() {
			out = append(out, h.f.ID())
		}
	}
	return out
}

func (m *MTR) SetUndo(txn, prevLSN uint64, payload []byte) error {
	if err := m.usable(); err != nil {
		return err
	}
	if m.hasUndo {
		return ErrUndoSet
	}
	m.undo = Undo{Txn: txn, PrevLSN: prevLSN, Payload: append([]byte(nil), payload...)}
	m.hasUndo = true
	return nil
}

func (m *MTR) Undo() (Undo, bool) {
	u := m.undo
	u.Payload = append([]byte(nil), u.Payload...)
	return u, m.hasUndo
}

func (m *MTR) Commit() error {
	if err := m.usable(); err != nil {
		return err
	}
	m.done = true
	for _, h := range m.holds {
		if h.changed() {
			m.pool.markDirty(h.f)
		}
	}
	errs := []error{m.release(false)}
	for _, id := range m.frees {
		errs = append(errs, m.pool.Free(id))
	}
	return errors.Join(errs...)
}

func (m *MTR) Abort() error {
	if err := m.usable(); err != nil {
		return err
	}
	m.done = true
	errs := []error{m.release(true)}
	for i := len(m.allocs) - 1; i >= 0; i-- {
		errs = append(errs, m.pool.Free(m.allocs[i]))
	}
	return errors.Join(errs...)
}

func (m *MTR) release(restore bool) error {
	for i := len(m.holds) - 1; i >= 0; i-- {
		h := m.holds[i]
		if restore && h.mode == exclusive {
			copy(h.f.buf, h.before)
			h.f.verified.Store(h.verified)
		}
		m.unlatch(h)
	}
	var errs []error
	for _, h := range m.holds {
		if err := m.pool.Unpin(h.f, false); err != nil {
			errs = append(errs, err)
		}
	}
	m.holds, m.byID = nil, map[core.PageID]*hold{}
	return errors.Join(errs...)
}
