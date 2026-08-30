package page

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/scute-db/scutedb/internal/core"
)

func TestHeaderRoundTrip(t *testing.T) {
	p := New(7, KindBTreeLeaf)
	p.SetItemCount(3)
	p.SetFlags(0x01)

	if got := p.ID(); got != 7 {
		t.Fatalf("ID = %d, want 7", got)
	}
	if got := p.Kind(); got != KindBTreeLeaf {
		t.Fatalf("Kind = %v, want btree-leaf", got)
	}
	if got := p.ItemCount(); got != 3 {
		t.Fatalf("ItemCount = %d, want 3", got)
	}
	if got := len(p); got != Size {
		t.Fatalf("page is %d bytes, want %d", got, Size)
	}
	if got := p.FreeSpace(); got != Usable {
		t.Fatalf("FreeSpace = %d, want %d", got, Usable)
	}
}

func TestHeaderByteLayout(t *testing.T) {
	p := New(0x01020304, KindHeap)
	p.SetItemCount(0x0506)

	want := []byte{
		0x01, 0x02, 0x03, 0x04,
		0x02,
		0x00,
		0x05, 0x06,
		0x00, 0x10,
		0x10, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	if !bytes.Equal(p[:HeaderSize], want) {
		t.Fatalf("header = % x\n          want % x", p[:HeaderSize], want)
	}
}

func TestAppendAndOverflow(t *testing.T) {
	p := New(1, KindHeap)
	at, err := p.Append([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if at != HeaderSize {
		t.Fatalf("first append landed at %d, want %d", at, HeaderSize)
	}
	if p.ItemCount() != 1 {
		t.Fatalf("ItemCount = %d, want 1", p.ItemCount())
	}
	if got := string(p[at : at+5]); got != "hello" {
		t.Fatalf("stored %q, want %q", got, "hello")
	}
	if _, err := p.Append(make([]byte, Usable)); err == nil {
		t.Fatal("expected overflow error, got nil")
	}
}

func TestFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round.db")

	pf, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p, err := pf.Allocate(KindHeap)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Append([]byte{byte('A' + i)}); err != nil {
			t.Fatal(err)
		}
		if err := pf.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := pf.Sync(); err != nil {
		t.Fatal(err)
	}
	pf.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 3*Size {
		t.Fatalf("file is %d bytes, want %d", st.Size(), 3*Size)
	}

	pf2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pf2.Close()
	for i := 0; i < 3; i++ {
		p, err := pf2.Read(core.PageID(i))
		if err != nil {
			t.Fatal(err)
		}
		if p.ID() != core.PageID(i) {
			t.Fatalf("page %d reports id %d", i, p.ID())
		}
		if got := p.Data()[0]; got != byte('A'+i) {
			t.Fatalf("page %d holds %q, want %q", i, got, byte('A'+i))
		}
	}
}

func TestRandomAccessIsConstantCost(t *testing.T) {
	if Offset(0) != 0 || Offset(1) != Size || Offset(500_000) != 500_000*Size {
		t.Fatal("page offsets are not a plain multiplication")
	}
}
