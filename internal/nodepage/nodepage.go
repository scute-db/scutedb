package nodepage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/page"
)

const (
	NodeHeaderSize = 8
	SlotSize       = 4
	SlotArrayStart = page.HeaderSize + NodeHeaderSize
	EntryArea      = page.Size - SlotArrayStart

	LeafSuffix     = 6
	InternalSuffix = 4
)

const (
	offLink  = page.HeaderSize
	offLevel = page.HeaderSize + 4
	offPad   = page.HeaderSize + 6
)

var (
	ErrNoFit     = errors.New("scutedb/nodepage: entry does not fit")
	ErrBadKind   = errors.New("scutedb/nodepage: not a b+tree node page")
	ErrBadLevel  = errors.New("scutedb/nodepage: level does not match kind")
	ErrInvariant = errors.New("scutedb/nodepage: invariant violated")
)

type Node struct{ page.Page }

func NewLeaf(id core.PageID) Node {
	p := page.New(id, page.KindBTreeLeaf)
	p.SetFreeStart(SlotArrayStart)
	return Node{p}
}

func NewInternal(id core.PageID, level uint16) (Node, error) {
	if level == 0 {
		return Node{}, fmt.Errorf("%w: internal node at level 0", ErrBadLevel)
	}
	p := page.New(id, page.KindBTreeInternal)
	p.SetFreeStart(SlotArrayStart)
	n := Node{p}
	n.setLevel(level)
	return n, nil
}

func Load(p page.Page) (Node, error) {
	if len(p) != page.Size {
		return Node{}, core.ErrShortPage
	}
	k := p.Kind()
	if k != page.KindBTreeLeaf && k != page.KindBTreeInternal {
		return Node{}, fmt.Errorf("%w: kind is %s", ErrBadKind, k)
	}
	return Node{p}, nil
}

func (n Node) Leaf() bool    { return n.Kind() == page.KindBTreeLeaf }
func (n Node) KeyCount() int { return int(n.ItemCount()) }
func (n Node) Level() uint16 { return binary.BigEndian.Uint16(n.Page[offLevel:]) }
func (n Node) ChildCount() int {
	if n.Leaf() {
		return 0
	}
	return n.KeyCount() + 1
}

func (n Node) setLevel(l uint16) { binary.BigEndian.PutUint16(n.Page[offLevel:], l) }

func (n Node) link() core.PageID      { return core.PageID(binary.BigEndian.Uint32(n.Page[offLink:])) }
func (n Node) setLink(id core.PageID) { binary.BigEndian.PutUint32(n.Page[offLink:], uint32(id)) }

func (n Node) Next() core.PageID            { return n.link() }
func (n Node) SetNext(id core.PageID)       { n.setLink(id) }
func (n Node) FirstChild() core.PageID      { return n.link() }
func (n Node) SetFirstChild(id core.PageID) { n.setLink(id) }

func (n Node) suffixLen() int {
	if n.Leaf() {
		return LeafSuffix
	}
	return InternalSuffix
}

func (n Node) slotOffset(i int) int { return SlotArrayStart + i*SlotSize }

func (n Node) cellBounds(i int) (int, int) {
	s := n.slotOffset(i)
	off := int(binary.BigEndian.Uint16(n.Page[s:]))
	length := int(binary.BigEndian.Uint16(n.Page[s+2:]))
	return off, off + length
}

func (n Node) cell(i int) []byte {
	lo, hi := n.cellBounds(i)
	return n.Page[lo:hi]
}

func (n Node) KeyRef(i int) []byte {
	c := n.cell(i)
	return c[:len(c)-n.suffixLen()]
}

func (n Node) Key(i int) []byte {
	return append([]byte(nil), n.KeyRef(i)...)
}

func (n Node) Row(i int) core.RowID {
	c := n.cell(i)
	tail := c[len(c)-LeafSuffix:]
	return core.RowID{
		Page: core.PageID(binary.BigEndian.Uint32(tail)),
		Slot: binary.BigEndian.Uint16(tail[4:]),
	}
}

func (n Node) Child(i int) core.PageID {
	if i == 0 {
		return n.FirstChild()
	}
	c := n.cell(i - 1)
	return core.PageID(binary.BigEndian.Uint32(c[len(c)-InternalSuffix:]))
}

func (n Node) EntrySize(keyLen int) int { return SlotSize + keyLen + n.suffixLen() }

func (n Node) Fits(keyLen int) bool { return n.EntrySize(keyLen) <= n.FreeSpace() }

func (n Node) appendCell(key []byte, tail []byte) error {
	need := SlotSize + len(key) + len(tail)
	if need > n.FreeSpace() {
		return fmt.Errorf("%w: needs %d bytes, %d free", ErrNoFit, need, n.FreeSpace())
	}
	start := int(n.FreeEnd()) - len(key) - len(tail)
	copy(n.Page[start:], key)
	copy(n.Page[start+len(key):], tail)

	s := n.slotOffset(n.KeyCount())
	binary.BigEndian.PutUint16(n.Page[s:], uint16(start))
	binary.BigEndian.PutUint16(n.Page[s+2:], uint16(len(key)+len(tail)))

	n.SetFreeEnd(uint16(start))
	n.SetFreeStart(uint16(s + SlotSize))
	n.SetItemCount(n.ItemCount() + 1)
	return nil
}

func (n Node) AppendLeaf(key []byte, rid core.RowID) error {
	if !n.Leaf() {
		return fmt.Errorf("%w: AppendLeaf on an internal node", ErrBadKind)
	}
	var tail [LeafSuffix]byte
	binary.BigEndian.PutUint32(tail[:], uint32(rid.Page))
	binary.BigEndian.PutUint16(tail[4:], rid.Slot)
	return n.appendCell(key, tail[:])
}

func (n Node) AppendInternal(key []byte, child core.PageID) error {
	if n.Leaf() {
		return fmt.Errorf("%w: AppendInternal on a leaf node", ErrBadKind)
	}
	var tail [InternalSuffix]byte
	binary.BigEndian.PutUint32(tail[:], uint32(child))
	return n.appendCell(key, tail[:])
}

func (n Node) Search(key []byte) (int, bool) {
	count := n.KeyCount()
	i := sort.Search(count, func(i int) bool { return bytes.Compare(n.KeyRef(i), key) >= 0 })
	if i < count && bytes.Equal(n.KeyRef(i), key) {
		return i, true
	}
	return i, false
}

func (n Node) ChildFor(key []byte) core.PageID {
	i := sort.Search(n.KeyCount(), func(i int) bool { return bytes.Compare(key, n.KeyRef(i)) < 0 })
	return n.Child(i)
}

func MaxKeys(kind page.Kind, keyLen int) int {
	suffix := InternalSuffix
	if kind == page.KindBTreeLeaf {
		suffix = LeafSuffix
	}
	per := SlotSize + keyLen + suffix
	if per <= 0 {
		return 0
	}
	return EntryArea / per
}

func Fanout(keyLen int) int { return MaxKeys(page.KindBTreeInternal, keyLen) + 1 }

func (n Node) Validate() error {
	k := n.Kind()
	if k != page.KindBTreeLeaf && k != page.KindBTreeInternal {
		return fmt.Errorf("%w: kind is %s", ErrBadKind, k)
	}
	if n.Leaf() != (n.Level() == 0) {
		return fmt.Errorf("%w: kind %s at level %d", ErrBadLevel, k, n.Level())
	}
	if got := binary.BigEndian.Uint16(n.Page[offPad:]); got != 0 {
		return fmt.Errorf("%w: padding bytes are %d, want 0", ErrInvariant, got)
	}

	count := n.KeyCount()
	wantStart := SlotArrayStart + count*SlotSize
	if int(n.FreeStart()) != wantStart {
		return fmt.Errorf("%w: free start is %d, %d slots end at %d",
			ErrInvariant, n.FreeStart(), count, wantStart)
	}
	if int(n.FreeEnd()) > page.Size || n.FreeEnd() < n.FreeStart() {
		return fmt.Errorf("%w: free end %d is outside [%d, %d]",
			ErrInvariant, n.FreeEnd(), n.FreeStart(), page.Size)
	}

	if count == 0 {
		if int(n.FreeEnd()) != page.Size {
			return fmt.Errorf("%w: empty node has free end %d, want %d",
				ErrInvariant, n.FreeEnd(), page.Size)
		}
		return nil
	}

	suffix := n.suffixLen()
	spans := make([][2]int, count)
	for i := 0; i < count; i++ {
		lo, hi := n.cellBounds(i)
		if lo < int(n.FreeStart()) || hi > page.Size || hi <= lo {
			return fmt.Errorf("%w: slot %d spans [%d, %d), outside [%d, %d)",
				ErrInvariant, i, lo, hi, n.FreeStart(), page.Size)
		}
		if hi-lo < suffix+1 {
			return fmt.Errorf("%w: slot %d holds %d bytes, too few for a %d-byte suffix and a key",
				ErrInvariant, i, hi-lo, suffix)
		}
		spans[i] = [2]int{lo, hi}
	}

	sort.Slice(spans, func(a, b int) bool { return spans[a][0] < spans[b][0] })
	if spans[0][0] != int(n.FreeEnd()) {
		return fmt.Errorf("%w: lowest cell starts at %d, free end says %d",
			ErrInvariant, spans[0][0], n.FreeEnd())
	}
	for i := 1; i < count; i++ {
		if spans[i-1][1] != spans[i][0] {
			return fmt.Errorf("%w: cells [%d, %d) and [%d, %d) are not adjacent",
				ErrInvariant, spans[i-1][0], spans[i-1][1], spans[i][0], spans[i][1])
		}
	}
	if spans[count-1][1] != page.Size {
		return fmt.Errorf("%w: highest cell ends at %d, want %d",
			ErrInvariant, spans[count-1][1], page.Size)
	}

	for i := 1; i < count; i++ {
		if bytes.Compare(n.KeyRef(i-1), n.KeyRef(i)) >= 0 {
			return fmt.Errorf("%w: key %d (%X) is not less than key %d (%X)",
				ErrInvariant, i-1, n.KeyRef(i-1), i, n.KeyRef(i))
		}
	}
	return nil
}
