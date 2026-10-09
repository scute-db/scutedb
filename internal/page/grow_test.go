package page

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
	"github.com/scute-db/scutedb/internal/fileio"
)

type growWrite struct {
	off int64
	n   int
}

type growFile struct {
	data    []byte
	writes  []growWrite
	failAt  int
	size    int64
	sizeErr error
}

var _ fileio.File = (*growFile)(nil)

var errGrowWrite = errors.New("growfile: injected write failure")

func (g *growFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(g.data)) {
		return 0, io.EOF
	}
	n := copy(p, g.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (g *growFile) WriteAt(p []byte, off int64) (int, error) {
	g.writes = append(g.writes, growWrite{off, len(p)})
	if g.failAt != 0 && len(g.writes) == g.failAt {
		return 0, errGrowWrite
	}
	if end := off + int64(len(p)); end > int64(len(g.data)) {
		g.data = append(g.data, make([]byte, end-int64(len(g.data)))...)
	}
	copy(g.data[off:], p)
	return len(p), nil
}

func (g *growFile) Sync() error  { return nil }
func (g *growFile) Close() error { return nil }

func (g *growFile) Size() (int64, error) {
	if g.sizeErr != nil {
		return 0, g.sizeErr
	}
	if g.size != 0 {
		return g.size, nil
	}
	return int64(len(g.data)), nil
}

func filledWith(b byte, pages int) *growFile {
	return &growFile{data: bytes.Repeat([]byte{b}, pages*Size)}
}

func TestGrowZeroesExactlyTheNewPagesAndNothingElse(t *testing.T) {
	g := filledWith(0xAA, 700)
	if err := NewFile(g).Grow(10, 610); err != nil {
		t.Fatal(err)
	}
	for p := 0; p < 700; p++ {
		want := byte(0xAA)
		if p >= 10 && p < 610 {
			want = 0
		}
		page := g.data[p*Size : (p+1)*Size]
		if !bytes.Equal(page, bytes.Repeat([]byte{want}, Size)) {
			t.Fatalf("page %d holds %02X..., want every byte %02X", p, page[0], want)
		}
	}
}

func TestGrowWritesInPiecesThatTileTheRange(t *testing.T) {
	g := &growFile{}
	if err := NewFile(g).Grow(10, 610); err != nil {
		t.Fatal(err)
	}
	if want := (600 + growPiece - 1) / growPiece; len(g.writes) != want {
		t.Fatalf("600 pages took %d writes, want %d pieces of at most %d pages", len(g.writes), want, growPiece)
	}
	next := Offset(10)
	for i, w := range g.writes {
		if w.off != next {
			t.Fatalf("write %d starts at %d, want %d: the pieces must be contiguous", i, w.off, next)
		}
		if w.n > growPiece*Size || w.n%Size != 0 {
			t.Fatalf("write %d is %d bytes, want whole pages and at most %d", i, w.n, growPiece*Size)
		}
		next += int64(w.n)
	}
	if next != Offset(610) {
		t.Fatalf("the pieces end at %d, want %d", next, Offset(610))
	}
}

func TestGrowOfOneChunkIsOneWrite(t *testing.T) {
	g := &growFile{}
	if err := NewFile(g).Grow(16, 32); err != nil {
		t.Fatal(err)
	}
	if len(g.writes) != 1 || g.writes[0] != (growWrite{Offset(16), 16 * Size}) {
		t.Fatalf("growing one 16-page chunk made writes %v, want a single 64 KB write at page 16", g.writes)
	}
}

func TestGrowDoesNotTrustTheFileSize(t *testing.T) {
	g := filledWith(0xAA, 40)
	if err := NewFile(g).Grow(16, 32); err != nil {
		t.Fatal(err)
	}
	if len(g.writes) != 1 {
		t.Fatalf("the file already reports 40 pages, but Grow(16, 32) must still write the zeros; writes: %v", g.writes)
	}
	if !bytes.Equal(g.data[16*Size:32*Size], make([]byte, 16*Size)) {
		t.Fatal("pages 16..31 are not zero after Grow")
	}
}

func TestGrowOfAnEmptyRangeWritesNothing(t *testing.T) {
	for _, r := range [][2]uint32{{5, 5}, {9, 3}, {0, 0}} {
		g := &growFile{}
		if err := NewFile(g).Grow(r[0], r[1]); err != nil {
			t.Fatal(err)
		}
		if len(g.writes) != 0 {
			t.Errorf("Grow(%d, %d) wrote %v, want nothing", r[0], r[1], g.writes)
		}
	}
}

func TestGrowStopsAtTheFirstFailedWrite(t *testing.T) {
	g := &growFile{failAt: 2}
	err := NewFile(g).Grow(0, 3*growPiece)
	if !errors.Is(err, errGrowWrite) {
		t.Fatalf("Grow with a failing second write returned %v, want the write error", err)
	}
	if len(g.writes) != 2 {
		t.Errorf("Grow kept writing after a failure: %d writes", len(g.writes))
	}
}

func TestPageCountClampsInsteadOfWrapping(t *testing.T) {
	g := &growFile{size: (int64(math.MaxUint32) + 17) * Size}
	n, err := NewFile(g).PageCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != math.MaxUint32 {
		t.Fatalf("a file of 2^32+17 pages reports %d pages; it must clamp to %d, not wrap to 16", n, uint32(math.MaxUint32))
	}
}

func TestKindNamesIncludeTheFreeList(t *testing.T) {
	if KindFreeList.String() != "free-list" {
		t.Errorf("KindFreeList prints as %q", KindFreeList.String())
	}
	if Kind(99).String() != "kind(99)" {
		t.Errorf("an unknown kind prints as %q", Kind(99).String())
	}
}

func TestAppendAcceptsAnExactFitAndRefusesOneByteMore(t *testing.T) {
	p := New(1, KindHeap)
	if _, err := p.Append(make([]byte, p.FreeSpace()+1)); err == nil {
		t.Fatal("Append of one byte more than the free space succeeded")
	}
	if _, err := p.Append(make([]byte, p.FreeSpace())); err != nil {
		t.Fatalf("Append of exactly the free space failed: %v", err)
	}
	if p.FreeSpace() != 0 {
		t.Fatalf("a page filled exactly has %d bytes free", p.FreeSpace())
	}
}

func TestReadOfAPageThatIsNotAllThereIsShort(t *testing.T) {
	g := &growFile{data: make([]byte, Size+Size/2)}
	pf := NewFile(g)
	if _, err := pf.Read(0); err != nil {
		t.Fatalf("page 0 is all there: %v", err)
	}
	if _, err := pf.Read(1); !errors.Is(err, core.ErrShortPage) && err == nil {
		t.Errorf("Read of a half-written page gave %v, want an error", err)
	}
	if _, err := pf.Read(9); err == nil {
		t.Error("Read of a page past the end of the file succeeded")
	}
}

func TestWriteRejectsAPageOfTheWrongSize(t *testing.T) {
	g := &growFile{}
	if err := NewFile(g).Write(make(Page, Size-1)); err == nil {
		t.Fatal("Write of a 4095-byte page succeeded")
	}
	if len(g.writes) != 0 {
		t.Errorf("a rejected page was still written: %v", g.writes)
	}
}

func TestPageCountAndAllocateReportSizeErrors(t *testing.T) {
	boom := errors.New("size failed")
	pf := NewFile(&growFile{sizeErr: boom})
	if _, err := pf.PageCount(); !errors.Is(err, boom) {
		t.Errorf("PageCount gave %v, want the size error", err)
	}
	if _, err := pf.Allocate(KindHeap); !errors.Is(err, boom) {
		t.Errorf("Allocate gave %v, want the size error", err)
	}
}

func TestAllocateReportsAFailedWrite(t *testing.T) {
	pf := NewFile(&growFile{failAt: 1})
	if _, err := pf.Allocate(KindHeap); !errors.Is(err, errGrowWrite) {
		t.Errorf("Allocate with a failing write gave %v, want the write error", err)
	}
}

func TestOpenInAMissingDirectoryFails(t *testing.T) {
	if _, err := Open(t.TempDir() + "/missing/dir/pages.db"); err == nil {
		t.Error("Open in a directory that does not exist succeeded")
	}
}

type eofAtEnd struct{ *growFile }

func (e eofAtEnd) ReadAt(p []byte, off int64) (int, error) {
	n, err := e.growFile.ReadAt(p, off)
	if err == nil && off+int64(n) == int64(len(e.data)) {
		return n, io.EOF
	}
	return n, err
}

type shortNoError struct{ *growFile }

func (s shortNoError) ReadAt(p []byte, off int64) (int, error) {
	n, _ := s.growFile.ReadAt(p[:len(p)/2], off)
	return n, nil
}

func TestReadAcceptsAFullPageThatAlsoReportsEndOfFile(t *testing.T) {
	g := filledWith(0x5A, 2)
	if _, err := NewFile(eofAtEnd{g}).Read(1); err != nil {
		t.Errorf("io.ReaderAt may return a full read together with io.EOF; Read rejected it: %v", err)
	}
}

func TestReadRejectsAShortPageEvenWithoutAnError(t *testing.T) {
	g := filledWith(0x5A, 2)
	if _, err := NewFile(shortNoError{g}).Read(0); !errors.Is(err, core.ErrShortPage) {
		t.Errorf("a half page returned with no error gave %v, want ErrShortPage", err)
	}
}
