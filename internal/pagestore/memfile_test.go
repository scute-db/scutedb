package pagestore

import (
	"errors"
	"io"

	"github.com/scute-db/scutedb/internal/fileio"
	"github.com/scute-db/scutedb/internal/page"
)

var errSyncFailed = errors.New("memfile: injected fsync failure")
var errMemClosed = errors.New("memfile: file is closed")

type memWrite struct {
	off  int64
	data []byte
}

type memFile struct {
	durable    []byte
	live       []byte
	log        []memWrite
	syncs      int
	failAtSync int
	failKeep   func(i int) bool
	closed     bool
}

var _ fileio.File = (*memFile)(nil)

func newMemFile() *memFile { return &memFile{} }

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if m.closed {
		return 0, errMemClosed
	}
	if off >= int64(len(m.live)) {
		return 0, io.EOF
	}
	n := copy(p, m.live[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	if m.closed {
		return 0, errMemClosed
	}
	m.live = writeInto(m.live, off, p)
	m.log = append(m.log, memWrite{off: off, data: append([]byte(nil), p...)})
	return len(p), nil
}

func writeInto(img []byte, off int64, p []byte) []byte {
	if end := off + int64(len(p)); end > int64(len(img)) {
		img = append(img, make([]byte, end-int64(len(img)))...)
	}
	copy(img[off:], p)
	return img
}

func (m *memFile) apply(keep func(i int) bool) {
	for i, w := range m.log {
		if keep == nil || keep(i) {
			m.durable = writeInto(m.durable, w.off, w.data)
		}
	}
	m.log = nil
}

func (m *memFile) Sync() error {
	if m.closed {
		return errMemClosed
	}
	m.syncs++
	if m.failAtSync != 0 && m.syncs == m.failAtSync {
		keep := m.failKeep
		if keep == nil {
			keep = func(int) bool { return false }
		}
		m.apply(keep)
		return errSyncFailed
	}
	m.apply(nil)
	return nil
}

func (m *memFile) Size() (int64, error) {
	if m.closed {
		return 0, errMemClosed
	}
	return int64(len(m.live)), nil
}

func (m *memFile) Close() error {
	m.closed = true
	return nil
}

func (m *memFile) resetFaults() {
	m.syncs = 0
	m.failAtSync = 0
	m.failKeep = nil
	m.closed = false
}

func (m *memFile) restart() { m.resetFaults() }

func (m *memFile) restartEvicting(evict func(block int) bool) {
	dirty := map[int]bool{}
	for _, w := range m.log {
		for b := int(w.off) / page.Size; b*page.Size < int(w.off)+len(w.data); b++ {
			dirty[b] = true
		}
	}
	for b := 0; b*page.Size < len(m.live); b++ {
		if dirty[b] || !evict(b) {
			continue
		}
		hi := (b + 1) * page.Size
		if hi > len(m.live) {
			hi = len(m.live)
		}
		for i := b * page.Size; i < hi; i++ {
			if i < len(m.durable) {
				m.live[i] = m.durable[i]
			} else {
				m.live[i] = 0
			}
		}
	}
	m.resetFaults()
}

func (m *memFile) powerLoss(keep func(i int) bool) {
	if keep == nil {
		keep = func(int) bool { return false }
	}
	m.apply(keep)
	m.live = append([]byte(nil), m.durable...)
	m.resetFaults()
}

func (m *memFile) keepNothing() { m.powerLoss(func(int) bool { return false }) }
