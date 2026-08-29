package page

import (
	"encoding/binary"
	"fmt"

	"github.com/suhailopensource/ScuteDB/internal/core"
	"github.com/suhailopensource/ScuteDB/internal/fileio"
)

const Size = 4096

const HeaderSize = 16

const Usable = Size - HeaderSize

type Kind uint8

const (
	KindFree Kind = iota
	KindMeta
	KindHeap
	KindBTreeLeaf
	KindBTreeInternal
)

func (k Kind) String() string {
	switch k {
	case KindFree:
		return "free"
	case KindMeta:
		return "meta"
	case KindHeap:
		return "heap"
	case KindBTreeLeaf:
		return "btree-leaf"
	case KindBTreeInternal:
		return "btree-internal"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

const (
	offID        = 0
	offKind      = 4
	offFlags     = 5
	offItemCount = 6
	offFreeStart = 8
	offFreeEnd   = 10
	offReserved  = 12
)

type Page []byte

func New(id core.PageID, kind Kind) Page {
	p := make(Page, Size)
	p.SetID(id)
	p.SetKind(kind)
	p.SetFreeStart(HeaderSize)
	p.SetFreeEnd(Size)
	return p
}

func (p Page) ID() core.PageID   { return core.PageID(binary.BigEndian.Uint32(p[offID:])) }
func (p Page) Kind() Kind        { return Kind(p[offKind]) }
func (p Page) Flags() uint8      { return p[offFlags] }
func (p Page) ItemCount() uint16 { return binary.BigEndian.Uint16(p[offItemCount:]) }
func (p Page) FreeStart() uint16 { return binary.BigEndian.Uint16(p[offFreeStart:]) }
func (p Page) FreeEnd() uint16   { return binary.BigEndian.Uint16(p[offFreeEnd:]) }
func (p Page) FreeSpace() int    { return int(p.FreeEnd()) - int(p.FreeStart()) }
func (p Page) Data() []byte      { return p[HeaderSize:] }

func (p Page) SetID(id core.PageID)  { binary.BigEndian.PutUint32(p[offID:], uint32(id)) }
func (p Page) SetKind(k Kind)        { p[offKind] = byte(k) }
func (p Page) SetFlags(f uint8)      { p[offFlags] = f }
func (p Page) SetItemCount(n uint16) { binary.BigEndian.PutUint16(p[offItemCount:], n) }
func (p Page) SetFreeStart(n uint16) { binary.BigEndian.PutUint16(p[offFreeStart:], n) }
func (p Page) SetFreeEnd(n uint16)   { binary.BigEndian.PutUint16(p[offFreeEnd:], n) }

func (p Page) Append(b []byte) (uint16, error) {
	if len(b) > p.FreeSpace() {
		return 0, fmt.Errorf("scutedb: %d bytes will not fit in %d free", len(b), p.FreeSpace())
	}
	at := p.FreeStart()
	copy(p[at:], b)
	p.SetFreeStart(at + uint16(len(b)))
	p.SetItemCount(p.ItemCount() + 1)
	return at, nil
}

func Offset(id core.PageID) int64 { return int64(id) * Size }

type File struct{ f fileio.File }

func Open(path string) (*File, error) {
	f, err := fileio.Open(path)
	if err != nil {
		return nil, err
	}
	return &File{f: f}, nil
}

func NewFile(f fileio.File) *File { return &File{f: f} }

func (pf *File) PageCount() (uint32, error) {
	size, err := pf.f.Size()
	if err != nil {
		return 0, err
	}
	return uint32(size / Size), nil
}

func (pf *File) Read(id core.PageID) (Page, error) {
	p := make(Page, Size)
	n, err := pf.f.ReadAt(p, Offset(id))
	if err != nil && n != Size {
		return nil, err
	}
	if n != Size {
		return nil, core.ErrShortPage
	}
	return p, nil
}

func (pf *File) Write(p Page) error {
	if len(p) != Size {
		return fmt.Errorf("scutedb: page is %d bytes, want %d", len(p), Size)
	}
	_, err := pf.f.WriteAt(p, Offset(p.ID()))
	return err
}

func (pf *File) Allocate(kind Kind) (Page, error) {
	n, err := pf.PageCount()
	if err != nil {
		return nil, err
	}
	p := New(core.PageID(n), kind)
	if err := pf.Write(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (pf *File) Sync() error  { return pf.f.Sync() }
func (pf *File) Close() error { return pf.f.Close() }
