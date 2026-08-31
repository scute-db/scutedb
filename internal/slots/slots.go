package slots

import (
	"errors"
	"fmt"

	"github.com/scute-db/scutedb/internal/page"
)

const DefaultAlign = 8

var (
	ErrNoFit      = errors.New("scutedb/slots: record does not fit in a page")
	ErrOutOfRange = errors.New("scutedb/slots: slot index out of range")
	ErrRecordSize = errors.New("scutedb/slots: record is not the layout's record size")
	ErrBadAlign   = errors.New("scutedb/slots: alignment must be a power of two")
	ErrZeroLayout = errors.New("scutedb/slots: layout was not built by NewLayout")
)

func Align(n, to int) int {
	if to <= 1 || n <= 0 {
		return n
	}
	r := n % to
	if r == 0 {
		return n
	}
	return n + to - r
}

func Padding(n, to int) int { return Align(n, to) - n }

func isPowerOfTwo(n int) bool { return n > 0 && n&(n-1) == 0 }

type Layout struct {
	recordSize uint16
	slotSize   uint16
	capacity   uint16
	base       uint16
	alignment  uint16
}

func NewLayout(recordSize, align int) (Layout, error) {
	if !isPowerOfTwo(align) {
		return Layout{}, fmt.Errorf("%w: %d", ErrBadAlign, align)
	}
	if recordSize <= 0 || recordSize > page.Size {
		return Layout{}, fmt.Errorf("%w: record size %d", ErrNoFit, recordSize)
	}
	base := Align(page.HeaderSize, align)
	slot := Align(recordSize, align)
	if slot <= 0 || base+slot > page.Size {
		return Layout{}, fmt.Errorf("%w: %d-byte record needs a %d-byte slot", ErrNoFit, recordSize, slot)
	}
	capacity := (page.Size - base) / slot
	if base+capacity*slot > page.Size {
		return Layout{}, ErrNoFit
	}
	return Layout{
		recordSize: uint16(recordSize),
		slotSize:   uint16(slot),
		capacity:   uint16(capacity),
		base:       uint16(base),
		alignment:  uint16(align),
	}, nil
}

func (l Layout) RecordSize() int { return int(l.recordSize) }
func (l Layout) SlotSize() int   { return int(l.slotSize) }
func (l Layout) Capacity() int   { return int(l.capacity) }
func (l Layout) Base() int       { return int(l.base) }
func (l Layout) Alignment() int  { return int(l.alignment) }

func (l Layout) Offset(i int) int { return int(l.base) + i*int(l.slotSize) }

func (l Layout) PadPerSlot() int { return int(l.slotSize) - int(l.recordSize) }

func (l Layout) Tail() int {
	return page.Size - int(l.base) - int(l.capacity)*int(l.slotSize)
}

func (l Layout) Waste() int { return l.PadPerSlot()*int(l.capacity) + l.Tail() }

func (l Layout) slotRange(p page.Page, i int) (int, int, error) {
	if l.slotSize == 0 || i < 0 || i >= int(l.capacity) {
		return 0, 0, l.rangeErr(p, i)
	}
	start := int(l.base) + i*int(l.slotSize)
	end := start + int(l.slotSize)
	if end > len(p) {
		return 0, 0, l.rangeErr(p, i)
	}
	return start, end, nil
}

func (l Layout) rangeErr(p page.Page, i int) error {
	if l.slotSize == 0 {
		return ErrZeroLayout
	}
	if i < 0 || i >= int(l.capacity) {
		return fmt.Errorf("%w: slot %d of %d", ErrOutOfRange, i, l.capacity)
	}
	end := int(l.base) + (i+1)*int(l.slotSize)
	return fmt.Errorf("%w: slot %d ends at byte %d of a %d-byte page",
		ErrNoFit, i, end, len(p))
}

func (l Layout) Put(p page.Page, i int, rec []byte) error {
	off, end, err := l.slotRange(p, i)
	if err != nil {
		return err
	}
	if len(rec) != int(l.recordSize) {
		return fmt.Errorf("%w: got %d, want %d", ErrRecordSize, len(rec), l.recordSize)
	}
	copy(p[off:end], rec)
	for j := off + len(rec); j < end; j++ {
		p[j] = 0
	}
	if uint16(i)+1 > p.ItemCount() {
		p.SetItemCount(uint16(i) + 1)
	}
	p.SetFreeStart(uint16(l.Offset(int(p.ItemCount()))))
	return nil
}

func (l Layout) GetRef(p page.Page, i int) ([]byte, error) {
	off, _, err := l.slotRange(p, i)
	if err != nil {
		return nil, err
	}
	return p[off : off+int(l.recordSize)], nil
}

func (l Layout) Get(p page.Page, i int) ([]byte, error) {
	ref, err := l.GetRef(p, i)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ref))
	copy(out, ref)
	return out, nil
}

func (l Layout) String() string {
	if l.slotSize == 0 {
		return "invalid layout"
	}
	return fmt.Sprintf("record %d -> slot %d (pad %d), %d per page, %d bytes wasted",
		l.recordSize, l.slotSize, l.PadPerSlot(), l.capacity, l.Waste())
}
