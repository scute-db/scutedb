package nodepage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

const MaxKeySize = 1000

var (
	ErrKeyTooLarge = errors.New("scutedb/nodepage: key is longer than MaxKeySize")
	ErrEmptyKey    = errors.New("scutedb/nodepage: key is empty")
	ErrOrder       = errors.New("scutedb/nodepage: key is out of order")
	ErrBadIndex    = errors.New("scutedb/nodepage: entry index out of range")
)

func CheckKey(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > MaxKeySize {
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrKeyTooLarge, len(key), MaxKeySize)
	}
	return nil
}

func (n Node) UsedBytes() int { return EntryArea - n.FreeSpace() }

func (n Node) InsertLeaf(i int, key []byte, rid core.RowID) error {
	if !n.Leaf() {
		return fmt.Errorf("%w: InsertLeaf on an internal node", ErrBadKind)
	}
	var tail [LeafSuffix]byte
	binary.BigEndian.PutUint32(tail[:], uint32(rid.Page))
	binary.BigEndian.PutUint16(tail[4:], rid.Slot)
	return n.insertCell(i, key, tail[:])
}

func (n Node) InsertInternal(i int, key []byte, child core.PageID) error {
	if n.Leaf() {
		return fmt.Errorf("%w: InsertInternal on a leaf node", ErrBadKind)
	}
	var tail [InternalSuffix]byte
	binary.BigEndian.PutUint32(tail[:], uint32(child))
	return n.insertCell(i, key, tail[:])
}

func (n Node) insertCell(i int, key, tail []byte) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	count := n.KeyCount()
	if i < 0 || i > count {
		return fmt.Errorf("%w: insert at %d, node holds %d", ErrBadIndex, i, count)
	}
	if i > 0 && bytes.Compare(n.KeyRef(i-1), key) >= 0 {
		return fmt.Errorf("%w: % 02X is not above key %d", ErrOrder, key, i-1)
	}
	if i < count && bytes.Compare(key, n.KeyRef(i)) >= 0 {
		return fmt.Errorf("%w: % 02X is not below key %d", ErrOrder, key, i)
	}
	need := SlotSize + len(key) + len(tail)
	if need > n.FreeSpace() {
		return fmt.Errorf("%w: needs %d bytes, %d free", ErrNoFit, need, n.FreeSpace())
	}

	start := int(n.FreeEnd()) - len(key) - len(tail)
	copy(n.Page[start:], key)
	copy(n.Page[start+len(key):], tail)

	at := n.slotOffset(i)
	end := n.slotOffset(count)
	copy(n.Page[at+SlotSize:end+SlotSize], n.Page[at:end])
	binary.BigEndian.PutUint16(n.Page[at:], uint16(start))
	binary.BigEndian.PutUint16(n.Page[at+2:], uint16(len(key)+len(tail)))

	n.SetFreeEnd(uint16(start))
	n.SetFreeStart(uint16(end + SlotSize))
	n.SetItemCount(uint16(count + 1))
	return nil
}

func (n Node) Remove(i int) error {
	count := n.KeyCount()
	if i < 0 || i >= count {
		return fmt.Errorf("%w: remove %d, node holds %d", ErrBadIndex, i, count)
	}
	lo, hi := n.cellBounds(i)
	low := int(n.FreeEnd())
	if lo < low || hi > page.Size || hi <= lo {
		return fmt.Errorf("%w: slot %d spans [%d, %d), cells live in [%d, %d)",
			ErrInvariant, i, lo, hi, low, page.Size)
	}
	size := hi - lo

	copy(n.Page[low+size:hi], n.Page[low:lo])
	for j := 0; j < count; j++ {
		s := n.slotOffset(j)
		off := int(binary.BigEndian.Uint16(n.Page[s:]))
		if j != i && off < lo {
			binary.BigEndian.PutUint16(n.Page[s:], uint16(off+size))
		}
	}
	clear(n.Page[low : low+size])

	at := n.slotOffset(i)
	end := n.slotOffset(count)
	copy(n.Page[at:end-SlotSize], n.Page[at+SlotSize:end])
	clear(n.Page[end-SlotSize : end])

	n.SetFreeEnd(uint16(low + size))
	n.SetFreeStart(uint16(end - SlotSize))
	n.SetItemCount(uint16(count - 1))
	return nil
}

func (n Node) SetRow(i int, rid core.RowID) error {
	if !n.Leaf() {
		return fmt.Errorf("%w: SetRow on an internal node", ErrBadKind)
	}
	if i < 0 || i >= n.KeyCount() {
		return fmt.Errorf("%w: row %d, node holds %d", ErrBadIndex, i, n.KeyCount())
	}
	c := n.cell(i)
	tail := c[len(c)-LeafSuffix:]
	binary.BigEndian.PutUint32(tail, uint32(rid.Page))
	binary.BigEndian.PutUint16(tail[4:], rid.Slot)
	return nil
}

func (n Node) SetChild(i int, id core.PageID) error {
	if n.Leaf() {
		return fmt.Errorf("%w: SetChild on a leaf node", ErrBadKind)
	}
	if i < 0 || i > n.KeyCount() {
		return fmt.Errorf("%w: child %d, node has %d", ErrBadIndex, i, n.ChildCount())
	}
	if i == 0 {
		n.SetFirstChild(id)
		return nil
	}
	c := n.cell(i - 1)
	binary.BigEndian.PutUint32(c[len(c)-InternalSuffix:], uint32(id))
	return nil
}
